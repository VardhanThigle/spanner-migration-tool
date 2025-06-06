// Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"math/bits"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/civil"
	sp "cloud.google.com/go/spanner"
	_ "github.com/lib/pq" // we will use database/sql package instead of using this package directly

	"github.com/GoogleCloudPlatform/spanner-migration-tool/common/constants"
	"github.com/GoogleCloudPlatform/spanner-migration-tool/internal"
	"github.com/GoogleCloudPlatform/spanner-migration-tool/profiles"
	"github.com/GoogleCloudPlatform/spanner-migration-tool/schema"
	"github.com/GoogleCloudPlatform/spanner-migration-tool/sources/common"
	"github.com/GoogleCloudPlatform/spanner-migration-tool/spanner/ddl"
	"github.com/GoogleCloudPlatform/spanner-migration-tool/streaming"
)

// InfoSchemaImpl postgres specific implementation for InfoSchema.
type InfoSchemaImpl struct {
	Db                 *sql.DB
	MigrationProjectId string
	SourceProfile      profiles.SourceProfile
	TargetProfile      profiles.TargetProfile
	IsSchemaUnique     *bool
}

func (isi InfoSchemaImpl) populateSchemaIsUnique(schemaAndNames []common.SchemaAndName) {
	schemaSet := make(map[string]struct{})
	for _, table := range schemaAndNames {
		schemaSet[table.Schema] = struct{}{}
	}
	if len(schemaSet) == 1 {
		*isi.IsSchemaUnique = true
	} else {
		*isi.IsSchemaUnique = false
	}
}

// StartChangeDataCapture is used for automatic triggering of Datastream job when
// performing a streaming migration.
func (isi InfoSchemaImpl) StartChangeDataCapture(ctx context.Context, conv *internal.Conv) (map[string]interface{}, error) {
	mp := make(map[string]interface{})
	var (
		schemaDetails map[string]internal.SchemaDetails
		err           error
	)
	commonInfoSchema := common.InfoSchemaImpl{}
	schemaDetails, err = commonInfoSchema.GetIncludedSrcTablesFromConv(conv)
	if err != nil {
		err = fmt.Errorf("error fetching the tableList to setup datastream migration, defaulting to all tables: %v", err)
	}
	streamingCfg, err := streaming.ReadStreamingConfig(isi.SourceProfile.Conn.Pg.StreamingConfig, isi.TargetProfile.Conn.Sp.Dbname, schemaDetails)
	if err != nil {
		return nil, fmt.Errorf("error reading streaming config: %v", err)
	}
	pubsubCfg, err := streaming.CreatePubsubResources(ctx, isi.MigrationProjectId, streamingCfg.DatastreamCfg.DestinationConnectionConfig, isi.TargetProfile.Conn.Sp.Dbname, constants.REGULAR_GCS)
	if err != nil {
		return nil, fmt.Errorf("error creating pubsub resources: %v", err)
	}
	streamingCfg.PubsubCfg = *pubsubCfg
	dlqPubsubCfg, err := streaming.CreatePubsubResources(ctx, isi.MigrationProjectId, streamingCfg.DatastreamCfg.DestinationConnectionConfig, isi.TargetProfile.Conn.Sp.Dbname, constants.DLQ_GCS)
	if err != nil {
		return nil, fmt.Errorf("error creating pubsub resources: %v", err)
	}
	streamingCfg.DlqPubsubCfg = *dlqPubsubCfg
	streamingCfg, err = streaming.StartDatastream(ctx, isi.MigrationProjectId, streamingCfg, isi.SourceProfile, isi.TargetProfile, schemaDetails)
	if err != nil {
		err = fmt.Errorf("error starting datastream: %v", err)
		return nil, err
	}
	mp["streamingCfg"] = streamingCfg
	return mp, err
}

// StartStreamingMigration is used for automatic triggering of Dataflow job when
// performing a streaming migration.
func (isi InfoSchemaImpl) StartStreamingMigration(ctx context.Context, migrationProjectId string, client *sp.Client, conv *internal.Conv, streamingInfo map[string]interface{}) (internal.DataflowOutput, error) {
	streamingCfg, _ := streamingInfo["streamingCfg"].(streaming.StreamingCfg)

	dfOutput, err := streaming.StartDataflow(ctx, migrationProjectId, isi.TargetProfile, streamingCfg, conv)
	if err != nil {
		err = fmt.Errorf("error starting dataflow: %v", err)
		return internal.DataflowOutput{}, err
	}
	return dfOutput, nil
}

// GetToDdl function below implement the common.InfoSchema interface.
func (isi InfoSchemaImpl) GetToDdl() common.ToDdl {
	return ToDdlImpl{}
}

// GetTableName returns table name.
func (isi InfoSchemaImpl) GetTableName(schema string, tableName string) string {
	if *isi.IsSchemaUnique { // Drop schema name as prefix if only one schema is detected.
		return tableName
	} else if schema == "public" {
		return tableName
	}
	return fmt.Sprintf("%s.%s", schema, tableName)
}

// GetRowsFromTable returns a sql Rows object for a table.
func (isi InfoSchemaImpl) GetRowsFromTable(conv *internal.Conv, tableId string) (interface{}, error) {
	// PostgreSQL schema and name can be arbitrary strings.
	// Ideally we would pass schema/name as a query parameter,
	// but PostgreSQL doesn't support this. So we quote it instead.
	isSchemaNamePrefixed := strings.HasPrefix(conv.SrcSchema[tableId].Name, conv.SrcSchema[tableId].Schema+".")
	var tableName string
	if isSchemaNamePrefixed {
		tableName = strings.TrimPrefix(conv.SrcSchema[tableId].Name, conv.SrcSchema[tableId].Schema+".")
	} else {
		tableName = conv.SrcSchema[tableId].Name
	}
	q := fmt.Sprintf(`SELECT * FROM "%s"."%s";`, conv.SrcSchema[tableId].Schema, tableName)
	rows, err := isi.Db.Query(q)
	if err != nil {
		return nil, err
	}
	return rows, err
}

// ProcessDataRows performs data conversion for source database
// 'db'. For each table, we extract data using a "SELECT *" query,
// convert the data to Spanner data (based on the source and Spanner
// schemas), and write it to Spanner.  If we can't get/process data
// for a table, we skip that table and process the remaining tables.
//
// Note that the database/sql library has a somewhat complex model for
// returning data from rows.Scan. Scalar values can be returned using
// the native value used by the underlying driver (by passing
// *interface{} to rows.Scan), or they can be converted to specific go
// types. Array values are always returned as []byte, a string
// encoding of the array values. This string encoding is
// database/driver specific. For example, for PostgreSQL, array values
// are returned in the form "{v1,v2,..,vn}", where each v1,v2,...,vn
// is a PostgreSQL encoding of the respective array value.
//
// We choose to do all type conversions explicitly ourselves so that
// we can generate more targeted error messages: hence we pass
// *interface{} parameters to row.Scan.
func (isi InfoSchemaImpl) ProcessData(conv *internal.Conv, tableId string, srcSchema schema.Table, colIds []string, spSchema ddl.CreateTable, additionalAttributes internal.AdditionalDataAttributes) error {
	srcTableName := conv.SrcSchema[tableId].Name
	rowsInterface, err := isi.GetRowsFromTable(conv, tableId)
	if err != nil {
		conv.Unexpected(fmt.Sprintf("Couldn't get data for table %s : err = %s", srcTableName, err))
		return err
	}
	rows := rowsInterface.(*sql.Rows)
	defer rows.Close()
	srcCols, _ := rows.Columns()
	v, iv := buildVals(len(srcCols))
	colNameIdMap := internal.GetSrcColNameIdMap(conv.SrcSchema[tableId])
	for rows.Next() {
		err := rows.Scan(iv...)
		if err != nil {
			conv.Unexpected(fmt.Sprintf("Couldn't process sql data row: %s", err))
			// Scan failed, so we don't have any data to add to bad rows.
			conv.StatsAddBadRow(srcTableName, conv.DataMode())
			continue
		}
		newValues, err1 := common.PrepareValues(conv, tableId, colNameIdMap, colIds, srcCols, v)
		cvtCols, cvtVals, err2 := convertSQLRow(conv, tableId, colIds, srcSchema, spSchema, newValues)
		if err1 != nil || err2 != nil {
			conv.Unexpected(fmt.Sprintf("Couldn't process sql data row: %s", err))
			conv.StatsAddBadRow(srcTableName, conv.DataMode())
			conv.CollectBadRow(srcTableName, srcCols, valsToStrings(v))
			continue
		}
		conv.WriteRow(srcTableName, conv.SpSchema[tableId].Name, cvtCols, cvtVals)
	}
	return nil
}

// ConvertSQLRow performs data conversion for a single row of data
// returned from a 'SELECT *' query. ConvertSQLRow assumes that
// srcCols, spCols and srcVals all have the same length. Note that
// ConvertSQLRow returns cols as well as converted values. This is
// because cols can change when we add a column (synthetic primary
// key) or because we drop columns (handling of NULL values).
func convertSQLRow(conv *internal.Conv, tableId string, colIds []string, srcSchema schema.Table, spSchema ddl.CreateTable, srcVals []interface{}) ([]string, []interface{}, error) {
	var vs []interface{}
	var cs []string
	for i, colId := range colIds {
		srcCd, ok1 := srcSchema.ColDefs[colId]
		spCd, ok2 := spSchema.ColDefs[colId]
		if !ok1 || !ok2 {
			return nil, nil, fmt.Errorf("data conversion: can't find schema for column id %s of table %s", colId, conv.SrcSchema[tableId].Name)
		}
		if srcVals[i] == nil {
			continue // Skip NULL values (nil is used by database/sql to represent NULL values).
		}
		var spVal interface{}
		var err error
		if spCd.T.IsArray {
			spVal, err = cvtSQLArray(conv, srcCd, spCd, srcVals[i])
		} else {
			spVal, err = cvtSQLScalar(conv, srcCd, spCd, srcVals[i])
		}
		if err != nil { // Skip entire row if we hit error.
			return nil, nil, fmt.Errorf("can't convert sql data for column id %s of table %s: %w", colIds, conv.SrcSchema[tableId].Name, err)
		}
		vs = append(vs, spVal)
		cs = append(cs, spCd.Name)
	}
	if aux, ok := conv.SyntheticPKeys[tableId]; ok {
		cs = append(cs, conv.SpSchema[tableId].ColDefs[aux.ColId].Name)
		vs = append(vs, fmt.Sprintf("%d", int64(bits.Reverse64(uint64(aux.Sequence)))))
		aux.Sequence++
		conv.SyntheticPKeys[tableId] = aux
	}
	return cs, vs, nil
}

// GetRowCount with number of rows in each table.
func (isi InfoSchemaImpl) GetRowCount(table common.SchemaAndName) (int64, error) {
	// PostgreSQL schema and name can be arbitrary strings.
	// Ideally we would pass schema/name as a query parameter,
	// but PostgreSQL doesn't support this. So we quote it instead.
	q := fmt.Sprintf(`SELECT COUNT(*) FROM "%s"."%s";`, table.Schema, table.Name)
	rows, err := isi.Db.Query(q)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var count int64
	if rows.Next() {
		err := rows.Scan(&count)
		return count, err
	}
	return 0, nil //Check if 0 is ok to return
}

// GetTables return list of tables in the selected database.
// TODO: All of the queries to get tables and table data should be in
// a single transaction to ensure we obtain a consistent snapshot of
// schema information and table data (pg_dump does something
// similar).
func (isi InfoSchemaImpl) GetTables() ([]common.SchemaAndName, error) {
	ignored := make(map[string]bool)
	// Ignore all system tables: we just want to convert user tables.
	for _, s := range []string{"information_schema", "postgres", "pg_catalog", "pg_temp_1", "pg_toast", "pg_toast_temp_1"} {
		ignored[s] = true
	}
	q := "SELECT table_schema, table_name FROM information_schema.tables where table_type = 'BASE TABLE'"
	rows, err := isi.Db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("couldn't get tables: %w", err)
	}
	defer rows.Close()
	var tableSchema, tableName string
	var tables []common.SchemaAndName
	for rows.Next() {
		rows.Scan(&tableSchema, &tableName)
		if !ignored[tableSchema] {
			tables = append(tables, common.SchemaAndName{Schema: tableSchema, Name: tableName})
		}
	}
	isi.populateSchemaIsUnique(tables)
	return tables, nil
}

// GetColumns returns a list of Column objects and names
func (isi InfoSchemaImpl) GetColumns(conv *internal.Conv, table common.SchemaAndName, constraints map[string][]string, primaryKeys []string) (map[string]schema.Column, []string, error) {
	q := `SELECT c.column_name, c.data_type, e.data_type, c.is_nullable, c.column_default, c.character_maximum_length, c.numeric_precision, c.numeric_scale
              FROM information_schema.COLUMNS c LEFT JOIN information_schema.element_types e
                 ON ((c.table_catalog, c.table_schema, c.table_name, 'TABLE', c.dtd_identifier)
                     = (e.object_catalog, e.object_schema, e.object_name, e.object_type, e.collection_type_identifier))
              where table_schema = $1 and table_name = $2 ORDER BY c.ordinal_position;`
	cols, err := isi.Db.Query(q, table.Schema, table.Name)
	if err != nil {
		return nil, nil, fmt.Errorf("couldn't get schema for table %s.%s: %s", table.Schema, table.Name, err)
	}
	defer cols.Close()
	colDefs := make(map[string]schema.Column)
	var colIds []string
	var colName, dataType, isNullable string
	var colDefault, elementDataType sql.NullString
	var charMaxLen, numericPrecision, numericScale sql.NullInt64
	for cols.Next() {
		err := cols.Scan(&colName, &dataType, &elementDataType, &isNullable, &colDefault, &charMaxLen, &numericPrecision, &numericScale)
		if err != nil {
			conv.Unexpected(fmt.Sprintf("Can't scan: %v", err))
			continue
		}
		ignored := schema.Ignored{}
		for _, c := range constraints[colName] {
			// c can be UNIQUE, PRIMARY KEY, FOREIGN KEY,
			// or CHECK (based on msql, sql server, postgres docs).
			// We've already filtered out PRIMARY KEY.
			switch c {
			case "CHECK":
				ignored.Check = true
			case "FOREIGN KEY", "PRIMARY KEY", "UNIQUE":
				// Nothing to do here -- these are handled elsewhere.
			}
		}
		ignored.Default = colDefault.Valid
		colId := internal.GenerateColumnId()
		c := schema.Column{
			Id:      colId,
			Name:    colName,
			Type:    toType(dataType, elementDataType, charMaxLen, numericPrecision, numericScale),
			NotNull: common.ToNotNull(conv, isNullable),
			Ignored: ignored,
		}
		colDefs[colId] = c
		colIds = append(colIds, colId)
	}
	return colDefs, colIds, nil
}

// GetConstraints returns a list of primary keys and by-column map of
// other constraints.  Note: we need to preserve ordinal order of
// columns in primary key constraints.
// Note that foreign key constraints are handled in getForeignKeys.
func (isi InfoSchemaImpl) GetConstraints(conv *internal.Conv, table common.SchemaAndName) ([]string, []schema.CheckConstraint, map[string][]string, error) {
	q := `SELECT k.COLUMN_NAME, t.CONSTRAINT_TYPE
              FROM INFORMATION_SCHEMA.TABLE_CONSTRAINTS AS t
                INNER JOIN INFORMATION_SCHEMA.KEY_COLUMN_USAGE AS k
                  ON t.CONSTRAINT_NAME = k.CONSTRAINT_NAME AND t.CONSTRAINT_SCHEMA = k.CONSTRAINT_SCHEMA
              WHERE k.TABLE_SCHEMA = $1 AND k.TABLE_NAME = $2 ORDER BY k.ordinal_position;`
	rows, err := isi.Db.Query(q, table.Schema, table.Name)
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()
	var primaryKeys []string
	var col, constraint string
	m := make(map[string][]string)
	for rows.Next() {
		err := rows.Scan(&col, &constraint)
		if err != nil {
			conv.Unexpected(fmt.Sprintf("Can't scan: %v", err))
			continue
		}
		if col == "" || constraint == "" {
			conv.Unexpected(fmt.Sprintf("Got empty col or constraint"))
			continue
		}
		switch constraint {
		case "PRIMARY KEY":
			primaryKeys = append(primaryKeys, col)
		default:
			m[col] = append(m[col], constraint)
		}
	}
	return primaryKeys, nil, m, nil
}

// GetForeignKeys returns a list of all the foreign key constraints.
func (isi InfoSchemaImpl) GetForeignKeys(conv *internal.Conv, table common.SchemaAndName) (foreignKeys []schema.ForeignKey, err error) {
	q := `SELECT
			rc.constraint_schema AS "TABLE_SCHEMA",
			ccu.table_name AS "REFERENCED_TABLE_NAME",
			kcu.column_name AS "COLUMN_NAME",
			ccu.column_name AS "REF_COLUMN_NAME",
			rc.constraint_name AS "CONSTRAINT_NAME",
			rc.delete_rule AS "ON_DELETE",
			rc.update_rule AS "ON_UPDATE"
		FROM
			INFORMATION_SCHEMA.REFERENTIAL_CONSTRAINTS rc
		INNER JOIN
			INFORMATION_SCHEMA.KEY_COLUMN_USAGE kcu
			ON rc.constraint_name = kcu.constraint_name
			AND rc.constraint_schema = kcu.constraint_schema
		INNER JOIN
			INFORMATION_SCHEMA.CONSTRAINT_COLUMN_USAGE ccu
			ON rc.constraint_name = ccu.constraint_name
			AND rc.constraint_schema = ccu.constraint_schema
		WHERE
			rc.constraint_schema = $1
			AND kcu.table_name = $2;`

	rows, err := isi.Db.Query(q, table.Schema, table.Name)

	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refTable common.SchemaAndName
	var col, refCol, fKeyName, onDelete, onUpdate string
	fKeys := make(map[string]common.FkConstraint)
	var keyNames []string
	for rows.Next() {
		err := rows.Scan(&refTable.Schema, &refTable.Name, &col, &refCol, &fKeyName, &onDelete, &onUpdate)
		if err != nil {
			conv.Unexpected(fmt.Sprintf("Can't scan: %v", err))
			continue
		}
		tableName := isi.GetTableName(refTable.Schema, refTable.Name)
		if _, found := fKeys[fKeyName]; found {
			fk := fKeys[fKeyName]
			fk.Cols = append(fk.Cols, col)
			fk.Refcols = append(fk.Refcols, refCol)
			fKeys[fKeyName] = fk
			fk.OnDelete = onDelete
			fk.OnUpdate = onUpdate
			continue
		}
		fKeys[fKeyName] = common.FkConstraint{Name: fKeyName, Table: tableName, Refcols: []string{refCol}, Cols: []string{col}, OnDelete: onDelete, OnUpdate: onUpdate}
		keyNames = append(keyNames, fKeyName)
	}

	sort.Strings(keyNames)
	for _, k := range keyNames {
		foreignKeys = append(foreignKeys,
			schema.ForeignKey{
				Id:               internal.GenerateForeignkeyId(),
				Name:             fKeys[k].Name,
				ColumnNames:      fKeys[k].Cols,
				ReferTableName:   fKeys[k].Table,
				ReferColumnNames: fKeys[k].Refcols,
				OnDelete:         fKeys[k].OnDelete,
				OnUpdate:         fKeys[k].OnUpdate,
			})
	}
	return foreignKeys, nil
}

// GetIndexes return a list of all indexes for the specified table.
// Note: Extracting index definitions from PostgreSQL information schema tables is complex.
// See https://stackoverflow.com/questions/6777456/list-all-index-names-column-names-and-its-table-name-of-a-postgresql-database/44460269#44460269
// for background.
func (isi InfoSchemaImpl) GetIndexes(conv *internal.Conv, table common.SchemaAndName, colNameIdMap map[string]string) ([]schema.Index, error) {
	q := `SELECT
			irel.relname AS index_name,
			a.attname AS column_name,
			1 + Array_position(i.indkey, a.attnum) AS column_position,
			i.indisunique AS is_unique,
			CASE o.OPTION & 1 WHEN 1 THEN 'DESC' ELSE 'ASC' END AS order
		FROM pg_index AS i
		JOIN pg_class AS trel
		ON trel.oid = i.indrelid
		JOIN pg_namespace AS tnsp
		ON trel.relnamespace = tnsp.oid
		JOIN pg_class AS irel
		ON irel.oid = i.indexrelid
		CROSS JOIN LATERAL UNNEST (i.indkey) WITH ordinality AS c (colnum, ordinality)
		LEFT JOIN LATERAL UNNEST (i.indoption) WITH ordinality AS o (OPTION, ordinality)
		ON c.ordinality = o.ordinality
		JOIN pg_attribute AS a
		ON trel.oid = a.attrelid
			AND a.attnum = c.colnum
		WHERE tnsp.nspname= $1
			AND trel.relname= $2
			AND i.indisprimary = false
		GROUP BY tnsp.nspname,
           		trel.relname,
           		irel.relname,
           		a.attname,
           		array_position(i.indkey, a.attnum),
           		o.OPTION,i.indisunique
		ORDER BY irel.relname, array_position(i.indkey, a.attnum);`
	rows, err := isi.Db.Query(q, table.Schema, table.Name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var name, column, sequence, isUnique, collation string
	indexMap := make(map[string]schema.Index)
	var indexNames []string
	var indexes []schema.Index
	for rows.Next() {
		if err := rows.Scan(&name, &column, &sequence, &isUnique, &collation); err != nil {
			conv.Unexpected(fmt.Sprintf("Can't scan: %v", err))
			continue
		}
		if _, found := indexMap[name]; !found {
			indexNames = append(indexNames, name)
			indexMap[name] = schema.Index{
				Id:     internal.GenerateIndexesId(),
				Name:   name,
				Unique: (isUnique == "true")}
		}
		index := indexMap[name]
		index.Keys = append(index.Keys, schema.Key{
			ColId: colNameIdMap[column],
			Desc:  (collation == "DESC")})
		indexMap[name] = index
	}
	for _, k := range indexNames {
		indexes = append(indexes, indexMap[k])
	}
	return indexes, nil
}

func toType(dataType string, elementDataType sql.NullString, charLen sql.NullInt64, numericPrecision, numericScale sql.NullInt64) schema.Type {
	switch {
	case dataType == "ARRAY" && elementDataType.Valid:
		return schema.Type{Name: elementDataType.String, ArrayBounds: []int64{-1}}
		// TODO: handle error cases.
		// TODO: handle case of multiple array bounds.
	case charLen.Valid:
		return schema.Type{Name: dataType, Mods: []int64{charLen.Int64}}
	case numericPrecision.Valid && numericScale.Valid && numericScale.Int64 != 0:
		return schema.Type{Name: dataType, Mods: []int64{numericPrecision.Int64, numericScale.Int64}}
	case numericPrecision.Valid:
		return schema.Type{Name: dataType, Mods: []int64{numericPrecision.Int64}}
	default:
		return schema.Type{Name: dataType}
	}
}

func cvtSQLArray(conv *internal.Conv, srcCd schema.Column, spCd ddl.ColumnDef, val interface{}) (interface{}, error) {
	a, ok := val.([]byte)
	if !ok {
		return nil, fmt.Errorf("can't convert array values to []byte")
	}
	return convArray(spCd.T, srcCd.Type.Name, conv.Location, string(a))
}

// cvtSQLScalar converts a values returned from a SQL query to a
// Spanner value.  In principle, we could just hand the values we get
// from the driver over to Spanner and have the Spanner client handle
// conversions and errors. However we handle the conversions
// explicitly ourselves so that we can generate more targeted error
// messages. Note that the caller is responsible for handling nil
// values (used to represent NULL). We handle each of the remaining
// cases of values returned by the database/sql library:
//
//	bool
//	[]byte
//	int64
//	float32
//	float64
//	string
//	time.Time
func cvtSQLScalar(conv *internal.Conv, srcCd schema.Column, spCd ddl.ColumnDef, val interface{}) (interface{}, error) {
	switch spCd.T.Name {
	case ddl.Bool:
		switch v := val.(type) {
		case bool:
			return v, nil
		case string:
			return convBool(v)
		}
	case ddl.Bytes:
		switch v := val.(type) {
		case []byte:
			return v, nil
		}
	case ddl.Date:
		// The PostgreSQL driver uses time.Time to represent
		// dates.  Note that the database/sql library doesn't
		// document how dates are represented, so maybe this
		// isn't a driver issue, but a generic database/sql
		// issue.  We explicitly convert from time.Time to
		// civil.Date (used by the Spanner client library).
		switch v := val.(type) {
		case string:
			return convDate(v)
		case time.Time:
			return civil.DateOf(v), nil
		}
	case ddl.Int64:
		switch v := val.(type) {
		case []byte: // Parse as int64.
			return convInt64(string(v))
		case int64:
			return v, nil
		case float32: // Truncate.
			return int64(v), nil
		case float64: // Truncate.
			return int64(v), nil
		case string: // Parse as int64.
			return convInt64(v)
		}
	case ddl.Float32:
		switch v := val.(type) {
		case []byte: // Note: PostgreSQL uses []byte for numeric.
			return convFloat32(string(v))
		case int64:
			return float32(v), nil
		case float32:
			return v, nil
		case float64:
			return float32(v), nil
		case string:
			return convFloat32(v)
		}
	case ddl.Float64:
		switch v := val.(type) {
		case []byte: // Note: PostgreSQL uses []byte for numeric.
			return convFloat64(string(v))
		case int64:
			return float64(v), nil
		case float32:
			return float64(v), nil
		case float64:
			return v, nil
		case string:
			return convFloat64(v)
		}
	case ddl.Numeric:
		switch v := val.(type) {
		case []byte: // Note: PostgreSQL uses []byte for numeric.
			return convNumeric(conv, string(v))
		}
	case ddl.String:
		switch v := val.(type) {
		case bool:
			return strconv.FormatBool(v), nil
		case []byte:
			return string(v), nil
		case int64:
			return strconv.FormatInt(v, 10), nil
		case float32:
			return strconv.FormatFloat(float64(v), 'g', -1, 32), nil
		case float64:
			return strconv.FormatFloat(v, 'g', -1, 64), nil
		case string:
			return v, nil
		case time.Time:
			return v.String(), nil
		}
	case ddl.Timestamp:
		switch v := val.(type) {
		case string:
			return convTimestamp(srcCd.Type.Name, conv.Location, v)
		case time.Time:
			return v, nil
		}
	case ddl.JSON:
		switch v := val.(type) {
		case string:
			return string(v), nil
		case []uint8:
			return string(v), nil
		}
	}
	return nil, fmt.Errorf("can't convert value of type %s to Spanner type %s", reflect.TypeOf(val), reflect.TypeOf(spCd.T))
}

// buildVals contructs interface{} value containers to scan row
// results into.  Returns both the underlying containers (as a slice)
// as well as an interface{} of pointers to containers to pass to
// rows.Scan.
func buildVals(n int) (v []interface{}, iv []interface{}) {
	v = make([]interface{}, n)
	for i := range v {
		iv = append(iv, &v[i])
	}
	return v, iv
}

func valsToStrings(vals []interface{}) []string {
	toString := func(val interface{}) string {
		if val == nil {
			return "NULL"
		}
		switch v := val.(type) {
		case *interface{}:
			val = *v
		}
		return fmt.Sprintf("%v", val)
	}
	var s []string
	for _, v := range vals {
		s = append(s, toString(v))
	}
	return s
}
