/*
	Copyright 2025 Google LLC

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
*/
package assessment

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io/ioutil"

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"github.com/GoogleCloudPlatform/spanner-migration-tool/logger"
	"go.uber.org/zap"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/types/known/structpb"
)

//go:embed go_concept_examples.json
var goMysqlMigrationConcept []byte

//go:embed java_concept_examples.json
var javaMysqlMigrationConcept []byte

type MySqlMigrationConcept struct {
	ID      string `json:"id"`
	Example string `json:"example"`
	Rewrite struct {
		Theory  string `json:"theory"`
		Options []struct {
			MySQLCode   string `json:"mysql_code"`
			SpannerCode string `json:"spanner_code"`
		} `json:"options"`
	} `json:"rewrite"`
	Embedding []float32 `json:"embedding,omitempty"`
}

func createEmbededTextsFromFile(project, location, language string) ([]MySqlMigrationConcept, error) {
	ctx := context.Background()
	apiEndpoint := fmt.Sprintf("%s-aiplatform.googleapis.com:443", location)
	model := "text-embedding-preview-0815"

	client, err := aiplatform.NewPredictionClient(ctx, option.WithEndpoint(apiEndpoint))
	if err != nil {
		return nil, err
	}
	defer client.Close()

	// Read the JSON file
	var data []byte
	switch language {
	case "go":
		data = goMysqlMigrationConcept
	case "java":
		data = javaMysqlMigrationConcept
	default:
		panic("Unsupported language")
	}

	var mysqlMigrationConcepts []MySqlMigrationConcept
	if err := json.Unmarshal(data, &mysqlMigrationConcepts); err != nil {
		return nil, err
	}

	instances := make([]*structpb.Value, len(mysqlMigrationConcepts))
	for i, concept := range mysqlMigrationConcepts {
		instances[i] = structpb.NewStructValue(&structpb.Struct{
			Fields: map[string]*structpb.Value{
				"content":   structpb.NewStringValue(concept.Example),
				"task_type": structpb.NewStringValue("SEMANTIC_SIMILARITY"),
			},
		})
	}

	req := &aiplatformpb.PredictRequest{
		Endpoint:  fmt.Sprintf("projects/%s/locations/%s/publishers/google/models/%s", project, location, model),
		Instances: instances,
	}

	resp, err := client.Predict(ctx, req)
	if err != nil {
		return nil, err
	}

	for i, prediction := range resp.Predictions {
		values := prediction.GetStructValue().Fields["embeddings"].GetStructValue().Fields["values"].GetListValue().Values
		embeddings := make([]float32, len(values))
		for j, value := range values {
			embeddings[j] = float32(value.GetNumberValue())
		}
		mysqlMigrationConcepts[i].Embedding = embeddings
	}
	return mysqlMigrationConcepts, nil
}

func embedTextsFromFile(project, location, inputPath, outputPath string) error {
	mysqlMigrationConcepts, err := createEmbededTextsFromFile(project, location, "java")
	if err != nil {
		return err
	}

	// Save updated data to a new JSON file
	outputData, err := json.MarshalIndent(mysqlMigrationConcepts, "", "  ")
	if err != nil {
		return err
	}

	if err := ioutil.WriteFile(outputPath, outputData, 0644); err != nil {
		return err
	}

	logger.Log.Debug("Embeddings saved to", zap.String("fkStmt", outputPath))
	return nil
}

// Sample Usage
// func main() {
// 	if err := embedTextsFromFile("", "", "go_concept_examples.json", "output.json"); err != nil {
// 		logger.Log.Debug("Error:", err)
// 	}
// }
