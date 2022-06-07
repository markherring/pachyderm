package main

import (
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/pachyderm/pachyderm/v2/src/internal/pachsql"
	"github.com/pachyderm/pachyderm/v2/src/internal/sdata"
	"github.com/sirupsen/logrus"
)

var log = logrus.StandardLogger()

func main() {
	// args[0]: operation (ingress, egress)
	// args[1]: file format (csv, json)
	// args[2]: table name
	// args[3]: url
	args := os.Args[1:]
	if len(args) < 4 {
		log.Fatal("Not enough arguments")
	}

	op, fileFormat, tableName, url := args[0], args[1], args[2], args[3]
	password, ok := os.LookupEnv("PACHYDERM_SQL_PASSWORD")
	if !ok {
		log.Fatalf("password missing")
	}
	u, err := pachsql.ParseURL(url)
	if err != nil {
		log.Fatal(err)
	}
	// tableName is <schema>.<table_name>
	tableNameSplitted := strings.Split(tableName, ".")
	schema, tableName := tableNameSplitted[0], tableNameSplitted[1]
	db, err := pachsql.OpenURL(*u, password)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	switch op {
	case "ingress":
		result, err := ingress(*db, fileFormat, schema, tableName)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(result)
	case "egress":
		egress()
	default:
		log.Fatalf("Unrecognized operation %s", op)
	}
}

func ingress(db pachsql.DB, fireFormat, schema, tableName string) (resultStats, error) {
	var result resultStats
	nRows, nColumns, size, err := getTableSize(db, schema, tableName)
	if err != nil {
		return result, err
	}
	start := time.Now()
	rows, err := db.Query(fmt.Sprintf("select * from %s.%s", schema, tableName))
	if err != nil {
		return result, err
	}
	// for now just use CSV
	dataOutDir, ok := os.LookupEnv("PACHYDERM_SQL_DATA_DIR")
	if !ok {
		dataOutDir = "/pfs/out/data"
	}
	if err = os.MkdirAll(dataOutDir, os.ModePerm); err != nil {
		return result, err
	}
	o, err := os.OpenFile(path.Join(dataOutDir, tableName), os.O_WRONLY|os.O_CREATE, 0755)
	if err != nil {
		return result, err
	}
	w := sdata.NewCSVWriter(o, nil)
	matResults, err := sdata.MaterializeSQL(w, rows)
	if err != nil {
		return result, err
	}
	if int(matResults.RowCount) != nRows {
		return result, fmt.Errorf("row counts don't match %d != %d", nRows, matResults.RowCount)
	}
	duration := time.Since(start)
	result.rows, result.columns, result.size, result.duration = nRows, nColumns, size, int(duration/time.Millisecond)
	return result, err
}

func egress() {
	fmt.Println("runnig egress")
}

type resultStats struct {
	rows, columns, size, duration int
}

func getTableSize(db pachsql.DB, schema, tableName string) (int, int, int, error) {
	var nrows, ncols, size int
	if err := db.QueryRow(fmt.Sprintf("select row_count, bytes from information_schema.tables where lower(table_schema) = lower('%s') and lower(table_name) = lower('%s')", schema, tableName)).Scan(&nrows, &size); err != nil {
		return 0, 0, 0, err
	}
	if err := db.QueryRow(fmt.Sprintf("select count(*) from information_schema.columns where lower(table_schema) = lower('%s') and lower(table_name) = lower('%s')", schema, tableName)).Scan(&ncols); err != nil {
		return 0, 0, 0, err
	}

	return nrows, ncols, size, nil
}
