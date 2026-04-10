package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	_ "github.com/go-sql-driver/mysql"
	opensearch "github.com/opensearch-project/opensearch-go"
)

// Prod config
const (
	elasticDomain   = "<elasticsearch prod url>"
	elasticUsername = "<username>"
	elasticPassword = "<password>"

	metaDBUser     = "testrail_meta"
	metaDBPassword = "<meta db password>"
	metaDBHost     = "<meta db host>"
	metaDBName     = "testrail_meta"

	batchSize = 1000
)

// // Stage Config
// const (
// 	elasticDomain   = "<elasticsearch stage url>"
// 	elasticUsername = "<username>"
// 	elasticPassword = "<password>"

// 	metaDBUser     = "testrail_meta"
// 	metaDBPassword = "<meta db password>"
// 	metaDBHost     = "<meta db host>"
// 	metaDBName     = "testrail_meta"

// 	batchSize = 1000
// )

// TrTables ...
var TrTables = map[string]int{
	"projects":              1,
	"cases":                 1,
	"runs":                  1,
	"milestones":            1,
	"suites":                1,
	"reports":               1,
	"cross_project_reports": 1,
}

// Document ...
type Document struct {
	Index      string
	DocumentID string
	Payload    string
}

// InstanceCreds ...
type InstanceCreds struct {
	Host     string
	DBName   string
	User     string
	Password string
}

func main() {
	if len(os.Args) < 2 {
		log.Fatalf("Usage: %s <instance_id>", os.Args[0])
	}
	instanceID, _ := strconv.Atoi(os.Args[1])
	index := fmt.Sprintf("testrail-%d", instanceID)

	metaDSN := fmt.Sprintf("%s:%s@tcp(%s:3306)/%s", metaDBUser, metaDBPassword, metaDBHost, metaDBName)
	metaDB, err := sql.Open("mysql", metaDSN)
	if err != nil {
		log.Fatalf("Failed to connect to meta DB: %v", err)
	}
	defer metaDB.Close()

	var creds InstanceCreds
	query := `
	SELECT 
		ds.name AS hostname,
		i.database_name,
		i.database_user,
		i.database_password
	FROM
		testrail_meta.instances i
		LEFT JOIN testrail_meta.database_servers ds ON i.database_server_id = ds.id
	WHERE i.id = ?
	`
	err = metaDB.QueryRow(query, instanceID).Scan(&creds.Host, &creds.DBName, &creds.User, &creds.Password)
	if err != nil {
		log.Fatalf("Failed to fetch instance DB credentials: %v", err)
	}
	log.Printf("Fetched DB credentials for instance %d: %s@%s/%s", instanceID, creds.User, creds.Host, creds.DBName)

	// Step 2: Connect to instance-specific DB
	instanceDSN := fmt.Sprintf("%s:%s@tcp(%s:3306)/%s", creds.User, creds.Password, creds.Host, creds.DBName)
	db, err := sql.Open("mysql", instanceDSN)
	if err != nil {
		log.Fatalf("Failed to connect to instance DB: %v", err)
	}
	defer db.Close()

	esCfg := opensearch.Config{
		Addresses: []string{
			elasticDomain,
		},
		Username: elasticUsername,
		Password: elasticPassword,
	}

	es, err := opensearch.NewClient(esCfg)
	if err != nil {
		log.Fatalf("Elasticsearch connection failed: %v", err)
	}

	for table := range TrTables {
		var lastID int64 = 0

		for {
			query := fmt.Sprintf("SELECT * FROM %s WHERE id > ? ORDER BY id ASC LIMIT ?", table)
			rows, err := db.Query(query, lastID, batchSize)
			if err != nil {
				log.Printf("Failed to query %s: %v", table, err)
				break
			}

			cols, _ := rows.Columns()
			// colTypes, _ := rows.ColumnTypes()

			var batchCount int
			for rows.Next() {
				values := make([]interface{}, len(cols))
				ptrs := make([]interface{}, len(cols))
				for i := range values {
					ptrs[i] = &values[i]
				}

				if err := rows.Scan(ptrs...); err != nil {
					log.Printf("Scan failed on table %s: %v", table, err)
					continue
				}

				// advance cursor (assumes first column is `id`)
				switch v := values[0].(type) {
				case int64:
					lastID = v
				case int32:
					lastID = int64(v)
				case int:
					lastID = int64(v)
				case []byte:
					if n, err := strconv.ParseInt(string(v), 10, 64); err == nil {
						lastID = n
					}
				default:
					if n, err := strconv.ParseInt(fmt.Sprintf("%v", v), 10, 64); err == nil {
						lastID = n
					}
				}

				payload := jsonify(values, cols, table)
				docID := fmt.Sprintf("%v_%s", values[0], table)

				doc := Document{
					Index:      index,
					DocumentID: docID,
					Payload:    payload,
				}

				if err := indexToES(es, doc); err != nil {
					log.Printf("Elasticsearch index failed: %v", err)
				} else {
					log.Printf("Synced %s with ID %s", table, docID)
				}

				batchCount++
				time.Sleep(500 * time.Millisecond)
			}
			rows.Close()

			// no more rows in this table
			if batchCount == 0 {
				break
			}
		}

		time.Sleep(10 * time.Second)

	}
}

func jsonify(row []interface{}, columns []string, tableName string) string {
	result := make(map[string]interface{}, len(columns))
	result["table_name"] = tableName

	for i, col := range columns {
		val := row[i]
		if b, ok := val.([]byte); ok {
			val = string(b)
		}
		if col == "created_on" {
			if tsInt, err := strconv.ParseInt(fmt.Sprintf("%v", val), 10, 64); err == nil {
				val = time.Unix(tsInt, 0).Format(time.RFC3339)
				result["@timestamp"] = val
			}
		}
		result[col] = val
	}

	jsonData, err := json.Marshal(result)
	if err != nil {
		log.Fatalf("Failed to marshal: %v", err)
	}
	return string(jsonData)
}

func indexToES(es *opensearch.Client, doc Document) error {
	res, err := es.Index(
		doc.Index,
		bytes.NewReader([]byte(doc.Payload)),
		es.Index.WithDocumentID(doc.DocumentID),
		es.Index.WithRefresh("true"),
	)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.IsError() {
		return fmt.Errorf("error indexing doc: %s", res.String())
	}
	return nil
}
