package postgres

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/lib/pq"
	"github.com/pganalyze/collector/state"
	"github.com/pganalyze/collector/util"
)

const statementSQLDefaultOptionalFields = "NULL, NULL, NULL, NULL, NULL"
const statementSQLpg94OptionalFields = "queryid, NULL, NULL, NULL, NULL"
const statementSQLpg95OptionalFields = "queryid, min_time, max_time, mean_time, stddev_time"

const statementSQL string = `
SELECT dbid, userid, query, calls, total_time, rows, shared_blks_hit, shared_blks_read,
			 shared_blks_dirtied, shared_blks_written, local_blks_hit, local_blks_read,
			 local_blks_dirtied, local_blks_written, temp_blks_read, temp_blks_written,
			 blk_read_time, blk_write_time, %s
	FROM %s
 WHERE query !~* '^%s' AND query <> '<insufficient privilege>'
			 AND query NOT LIKE 'DEALLOCATE %%'`

const statementStatsHelperSQL string = `
SELECT 1 AS enabled
	FROM pg_proc
	JOIN pg_namespace ON (pronamespace = pg_namespace.oid)
 WHERE nspname = 'pganalyze' AND proname = 'get_stat_statements'
`

func statementStatsHelperExists(db *sql.DB) bool {
	var enabled bool

	err := db.QueryRow(QueryMarkerSQL + statementStatsHelperSQL).Scan(&enabled)
	if err != nil {
		return false
	}

	return enabled
}

func GetStatements(logger *util.Logger, db *sql.DB, postgresVersion state.PostgresVersion) (map[state.PostgresStatementKey]state.PostgresStatement, error) {
	var err error
	var optionalFields string
	var sourceTable string

	if postgresVersion.Numeric >= state.PostgresVersion95 {
		optionalFields = statementSQLpg95OptionalFields
	} else if postgresVersion.Numeric >= state.PostgresVersion94 {
		optionalFields = statementSQLpg94OptionalFields
	} else {
		optionalFields = statementSQLDefaultOptionalFields
	}

	if statementStatsHelperExists(db) {
		logger.PrintVerbose("Found pganalyze.get_stat_statements() stats helper")
		sourceTable = "pganalyze.get_stat_statements()"
	} else {
		sourceTable = "pg_stat_statements"
	}

	queryMarkerRegex := strings.Trim(QueryMarkerSQL, " ")
	queryMarkerRegex = strings.Replace(queryMarkerRegex, "*", "\\*", -1)
	queryMarkerRegex = strings.Replace(queryMarkerRegex, "/", "\\/", -1)

	sql := QueryMarkerSQL + fmt.Sprintf(statementSQL, optionalFields, sourceTable, queryMarkerRegex)

	stmt, err := db.Prepare(sql)
	if err != nil {
		if sourceTable == "pg_stat_statements" && err.(*pq.Error).Code == "42P01" { // undefined_table
			logger.PrintInfo("pg_stat_statements relation does not exist, trying to create extension...")

			_, err = db.Exec(QueryMarkerSQL + "CREATE EXTENSION IF NOT EXISTS pg_stat_statements")
			if err != nil {
				return nil, err
			}

			stmt, err = db.Prepare(sql)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	defer stmt.Close()

	rows, err := stmt.Query()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	statements := make(map[state.PostgresStatementKey]state.PostgresStatement)

	for rows.Next() {
		var statement state.PostgresStatement

		err = rows.Scan(&statement.DatabaseOid, &statement.UserOid, &statement.NormalizedQuery, &statement.Calls, &statement.TotalTime, &statement.Rows,
			&statement.SharedBlksHit, &statement.SharedBlksRead, &statement.SharedBlksDirtied, &statement.SharedBlksWritten,
			&statement.LocalBlksHit, &statement.LocalBlksRead, &statement.LocalBlksDirtied, &statement.LocalBlksWritten,
			&statement.TempBlksRead, &statement.TempBlksWritten, &statement.BlkReadTime, &statement.BlkWriteTime,
			&statement.QueryId, &statement.MinTime, &statement.MaxTime, &statement.MeanTime, &statement.StddevTime)
		if err != nil {
			return nil, err
		}

		statements[statement.Key()] = statement
	}

	return statements, nil
}