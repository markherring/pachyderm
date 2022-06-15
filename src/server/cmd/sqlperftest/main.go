package main

import (
	"context"
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

	op, fileFormat, filename, url := args[0], args[1], args[2], args[3]
	password, ok := os.LookupEnv("PACHYDERM_SQL_PASSWORD")
	if !ok {
		log.Fatalf("password missing")
	}
	u, err := pachsql.ParseURL(url)
	if err != nil {
		log.Fatal(err)
	}
	// tableName is <schema>.<table_name>
	tableNameSplitted := strings.Split(path.Base(filename), ".")
	schema, filename := tableNameSplitted[0], tableNameSplitted[1]
	db, err := pachsql.OpenURL(*u, password)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	var result resultStats
	switch op {
	case "ingress":
		log.Info("beginning ingress")
		result, err = ingress(*db, fileFormat, schema, filename)
		if err != nil {
			log.Fatal(err)
		}
		log.Info("finished ingress")
	case "egress":
		log.Info("beginning egress")
		result, err = egress(*db, fileFormat, schema, filename)
		if err != nil {
			log.Fatal(err)
		}
		log.Info("finished egress")
	default:
		log.Fatalf("Unrecognized operation %s", op)
	}
	fmt.Println(result)
}

func ingress(db pachsql.DB, fileFormat, schema, tableName string) (result resultStats, err error) {
	nRows, nColumns, size, err := getTableSize(db, schema, tableName)
	if err != nil {
		return result, err
	}
	w, err := getWriter(schema, tableName)

	start := time.Now()
	rows, err := db.Query(fmt.Sprintf("select * from %s.%s", schema, tableName))
	if err != nil {
		return result, err
	}
	defer func() {
		err = rows.Close()
	}()
	matResults, err := sdata.MaterializeSQL(w, rows)
	if err != nil {
		return result, err
	}
	if int(matResults.RowCount) != nRows {
		return result, fmt.Errorf("row counts don't match %d != %d", nRows, matResults.RowCount)
	}
	duration := time.Since(start) / time.Millisecond

	result.rows, result.columns, result.size, result.duration = nRows, nColumns, size, int(duration)
	return result, err
}

func egress(db pachsql.DB, fileFormat, schema, tableName string) (result resultStats, err error) {
	start := time.Now()
	ctx := context.Background()
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	r, err := getReader(schema, tableName)
	if err != nil {
		return result, err
	}
	tablePath := fmt.Sprintf("%s.%s", schema, tableName)
	log.Info("dropping table", tablePath)
	_, err = tx.Exec(fmt.Sprintf("DELETE FROM %s", tablePath))
	if err != nil {
		return result, err
	}
	log.Info("dropped table", tablePath)
	tableInfo, err := pachsql.GetTableInfoTx(tx, tablePath)
	if err != nil {
		return result, err
	}
	w := sdata.NewSQLTupleWriter(tx, tableInfo)
	tuple, err := sdata.NewTupleFromTableInfo(tableInfo)
	if err != nil {
		return result, err
	}
	log.Info("starting to copy data")
	if _, err := sdata.Copy(w, r, tuple); err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		log.Error("commit failed")
		return result, err
	}
	log.Info("finished copying data")
	duration := time.Since(start) / time.Millisecond
	nRows, nCols, size, err := getTableSize(db, schema, tableName)
	if err != nil {
		return result, err
	}
	result.rows, result.columns, result.size, result.duration = nRows, nCols, size, int(duration)
	return result, err
}

type resultStats struct {
	rows, columns, size, duration int
}

func (r resultStats) String() string {
	return fmt.Sprintf("%d,%d,%d,%d", r.rows, r.columns, r.size, r.duration)
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

func getWriter(schema, tableName string) (sdata.TupleWriter, error) {
	// for now just use CSV
	dataOutDir, ok := os.LookupEnv("PACHYDERM_SQL_DATA_DIR")
	if !ok {
		dataOutDir = "/pfs/out/data"
	}
	if err := os.MkdirAll(dataOutDir, os.ModePerm); err != nil {
		return nil, err
	}
	o, err := os.OpenFile(path.Join(dataOutDir, fmt.Sprintf("%s.%s", schema, tableName)), os.O_WRONLY|os.O_CREATE, 0755)
	if err != nil {
		return nil, err
	}
	return sdata.NewCSVWriter(o, nil), nil
}

func getReader(schema, tableName string) (sdata.TupleReader, error) {
	inputDir, ok := os.LookupEnv("PACHYDERM_SQL_DATA_DIR")
	if !ok {
		inputDir = "/pfs/input/data"
	}
	fileName := fmt.Sprintf("%s.%s", schema, tableName)
	filePath := path.Join(inputDir, fileName)
	o, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	return sdata.NewCSVParser(o), nil
}
