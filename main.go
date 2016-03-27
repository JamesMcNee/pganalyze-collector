package main

import (
	"bytes"
	"compress/zlib"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"os/user"
	"strconv"
	"sync"
	"syscall"
	"time"

	flag "github.com/ogier/pflag"

	"database/sql"

	_ "github.com/lib/pq" // Enable database package to use Postgres

	"github.com/pganalyze/collector/config"
	"github.com/pganalyze/collector/dbstats"
	"github.com/pganalyze/collector/explain"
	"github.com/pganalyze/collector/logs"
	scheduler "github.com/pganalyze/collector/scheduler"
	systemstats "github.com/pganalyze/collector/systemstats"
	"github.com/pganalyze/collector/util"
)

type snapshot struct {
	ActiveQueries []dbstats.Activity          `json:"backends"`
	Statements    []dbstats.Statement         `json:"queries"`
	Postgres      snapshotPostgres            `json:"postgres"`
	System        *systemstats.SystemSnapshot `json:"system"`
	Logs          []logs.Line                 `json:"logs"`
	Explains      []explain.Explain           `json:"explains"`
}

type snapshotPostgres struct {
	Relations []dbstats.Relation `json:"schema"`
	Settings  []dbstats.Setting  `json:"settings"`
	Functions []dbstats.Function `json:"functions"`
}

type collectionOpts struct {
	collectPostgresRelations bool
	collectPostgresSettings  bool
	collectPostgresLocks     bool
	collectPostgresFunctions bool
	collectPostgresBloat     bool
	collectPostgresViews     bool

	collectLogs              bool
	collectExplain           bool
	collectSystemInformation bool

	submitCollectedData bool
	testRun             bool
}

func collectStatistics(config config.DatabaseConfig, db *sql.DB, collectionOpts collectionOpts, logger *util.Logger) (err error) {
	var stats snapshot
	var explainInputs []explain.ExplainInput
	var postgresVersion string
	var postgresVersionReadable string
	var postgresVersionNum int

	err = db.QueryRow(dbstats.QueryMarkerSQL + "SELECT version()").Scan(&postgresVersion)
	if err != nil {
		return
	}

	err = db.QueryRow(dbstats.QueryMarkerSQL + "SHOW server_version").Scan(&postgresVersionReadable)
	if err != nil {
		return
	}

	err = db.QueryRow(dbstats.QueryMarkerSQL + "SHOW server_version_num").Scan(&postgresVersionNum)
	if err != nil {
		return
	}

	logger.PrintVerbose("Detected PostgreSQL Version %d (%s)", postgresVersionNum, postgresVersion)

	if postgresVersionNum < dbstats.MinRequiredPostgresVersion {
		err = fmt.Errorf("Error: Your PostgreSQL server version (%s) is too old, 9.2 or newer is required.", postgresVersionReadable)
		return
	}

	stats.ActiveQueries, err = dbstats.GetActivity(logger, db, postgresVersionNum)
	if err != nil {
		return
	}

	stats.Statements, err = dbstats.GetStatements(logger, db, postgresVersionNum)
	if err != nil {
		return
	}

	if collectionOpts.collectPostgresRelations {
		stats.Postgres.Relations, err = dbstats.GetRelations(db, postgresVersionNum, collectionOpts.collectPostgresBloat)
		if err != nil {
			return
		}
	}

	if collectionOpts.collectPostgresSettings {
		stats.Postgres.Settings, err = dbstats.GetSettings(db, postgresVersionNum)
		if err != nil {
			return
		}
	}

	if collectionOpts.collectPostgresFunctions {
		stats.Postgres.Functions, err = dbstats.GetFunctions(db, postgresVersionNum)
		if err != nil {
			return
		}
	}

	if collectionOpts.collectSystemInformation {
		stats.System = systemstats.GetSystemSnapshot(config)
	}

	if collectionOpts.collectLogs {
		stats.Logs, explainInputs = logs.GetLogLines(config)

		if collectionOpts.collectExplain {
			stats.Explains = explain.RunExplain(db, explainInputs)
		}
	}

	statsJSON, _ := json.Marshal(stats)

	if !collectionOpts.submitCollectedData {
		var out bytes.Buffer
		json.Indent(&out, statsJSON, "", "\t")
		logger.PrintInfo("Dry run - JSON data that would have been sent will be output on stdout:\n")
		fmt.Print(out.String())
		return
	}

	var compressedJSON bytes.Buffer
	w := zlib.NewWriter(&compressedJSON)
	w.Write(statsJSON)
	w.Close()

	resp, err := http.PostForm(config.APIURL, url.Values{
		"data":               {compressedJSON.String()},
		"data_compressor":    {"zlib"},
		"api_key":            {config.APIKey},
		"submitter":          {"pganalyze-collector 0.9.0rc2"},
		"system_information": {"false"},
		"no_reset":           {"true"},
		"query_source":       {"pg_stat_statements"},
		"collected_at":       {fmt.Sprintf("%d", time.Now().Unix())},
	})
	// TODO: We could consider re-running on error (e.g. if it was a temporary server issue)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return
	}

	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("Error when submitting: %s\n", body)
		return
	}

	logger.PrintInfo("Submitted snapshot successfully")
	return
}

func collectAllDatabases(databases []configAndConnection, globalCollectionOpts collectionOpts, logger *util.Logger) {
	for _, database := range databases {
		prefixedLogger := logger.WithPrefix(database.config.SectionName)
		err := collectStatistics(database.config, database.connection, globalCollectionOpts, prefixedLogger)
		if err != nil {
			prefixedLogger.PrintError("%s", err)
		}
	}
}

func connectToDb(config config.DatabaseConfig, logger *util.Logger) (*sql.DB, error) {
	connectString := config.GetPqOpenString()
	logger.PrintVerbose("sql.Open(\"postgres\", \"%s\")", connectString)

	db, err := sql.Open("postgres", connectString)
	if err != nil {
		return nil, err
	}

	err = db.Ping()
	if err != nil {
		return nil, err
	}

	return db, nil
}

type configAndConnection struct {
	config     config.DatabaseConfig
	connection *sql.DB
}

func establishConnection(config config.DatabaseConfig, logger *util.Logger) (database configAndConnection, err error) {
	database = configAndConnection{config: config}
	requestedSslMode := config.DbSslMode

	// Go's lib/pq does not support sslmode properly, so we have to implement the "prefer" mode ourselves
	if requestedSslMode == "prefer" {
		config.DbSslMode = "require"
	}

	database.connection, err = connectToDb(config, logger)
	if err != nil {
		if err.Error() == "pq: SSL is not enabled on the server" && requestedSslMode == "prefer" {
			config.DbSslMode = "disable"
			database.connection, err = connectToDb(config, logger)
		}
	}

	return
}

func run(wg sync.WaitGroup, globalCollectionOpts collectionOpts, logger *util.Logger, configFilename string) chan<- bool {
	var databases []configAndConnection

	schedulerGroups, err := scheduler.ReadSchedulerGroups(scheduler.DefaultConfig)
	if err != nil {
		logger.PrintError("Error: Could not read scheduler groups, awaiting SIGHUP or process kill")
		return nil
	}

	databaseConfigs, err := config.Read(configFilename)
	if err != nil {
		logger.PrintError("Error: Could not read configuration, awaiting SIGHUP or process kill")
		return nil
	}

	for _, config := range databaseConfigs {
		prefixedLogger := logger.WithPrefix(config.SectionName)
		database, err := establishConnection(config, prefixedLogger)
		if err != nil {
			prefixedLogger.PrintError("Error: Failed to connect to database: %s", err)
		} else {
			databases = append(databases, database)
		}
	}

	// We intentionally don't do a test-run in the normal mode, since we're fine with
	// a later SIGHUP that fixes the config (or a temporarily unreachable server at start)
	if globalCollectionOpts.testRun {
		collectAllDatabases(databases, globalCollectionOpts, logger)
		return nil
	}

	stop := schedulerGroups["stats"].Schedule(func() {
		wg.Add(1)
		collectAllDatabases(databases, globalCollectionOpts, logger)
		wg.Done()
	}, logger, "collection of all databases")

	return stop
}

func main() {
	var dryRun bool
	var testRun bool
	var configFilename string
	var pidFilename string
	var noPostgresSettings, noPostgresLocks, noPostgresFunctions, noPostgresBloat, noPostgresViews bool
	var noPostgresRelations, noLogs, noExplain, noSystemInformation bool

	logger := &util.Logger{Destination: log.New(os.Stderr, "", log.LstdFlags)}

	usr, err := user.Current()
	if err != nil {
		logger.PrintError("Could not get user context from operating system - can't initialize, exiting.")
		return
	}

	flag.BoolVarP(&testRun, "test", "t", false, "Tests whether we can successfully collect data, submits it to the server, and exits afterwards.")
	flag.BoolVarP(&logger.Verbose, "verbose", "v", false, "Outputs additional debugging information, use this if you're encoutering errors or other problems.")
	flag.BoolVar(&dryRun, "dry-run", false, "Print JSON data that would get sent to web service (without actually sending) and exit afterwards.")
	flag.BoolVar(&noPostgresRelations, "no-postgres-relations", false, "Don't collect any Postgres relation information (not recommended)")
	flag.BoolVar(&noPostgresSettings, "no-postgres-settings", false, "Don't collect Postgres configuration settings")
	flag.BoolVar(&noPostgresLocks, "no-postgres-locks", false, "Don't collect Postgres lock information (NOTE: This is always enabled right now, i.e. no lock data is gathered)")
	flag.BoolVar(&noPostgresFunctions, "no-postgres-functions", false, "Don't collect Postgres function/procedure information")
	flag.BoolVar(&noPostgresBloat, "no-postgres-bloat", false, "Don't collect Postgres table/index bloat statistics")
	flag.BoolVar(&noPostgresViews, "no-postgres-views", false, "Don't collect Postgres view/materialized view information (NOTE: This is not implemented right now - views are always collected)")
	flag.BoolVar(&noLogs, "no-logs", false, "Don't collect log data")
	flag.BoolVar(&noExplain, "no-explain", false, "Don't automatically EXPLAIN slow queries logged in the logfile")
	flag.BoolVar(&noSystemInformation, "no-system-information", false, "Don't collect OS level performance data")
	flag.StringVar(&configFilename, "config", usr.HomeDir+"/.pganalyze_collector.conf", "Specify alternative path for config file.")
	flag.StringVar(&pidFilename, "pidfile", "", "Specifies a path that a pidfile should be written to. (default is no pidfile being written)")
	flag.Parse()

	globalCollectionOpts := collectionOpts{
		submitCollectedData:      true,
		testRun:                  true,
		collectPostgresRelations: !noPostgresRelations,
		collectPostgresSettings:  !noPostgresSettings,
		collectPostgresLocks:     !noPostgresLocks,
		collectPostgresFunctions: !noPostgresFunctions,
		collectPostgresBloat:     !noPostgresBloat,
		collectPostgresViews:     !noPostgresViews,
		collectLogs:              !noLogs,
		collectExplain:           !noExplain,
		collectSystemInformation: !noSystemInformation,
	}

	if dryRun {
		globalCollectionOpts.submitCollectedData = false
		globalCollectionOpts.testRun = true
	} else {
		// Check some cases we can't support from a pganalyze perspective right now
		if noPostgresRelations {
			logger.PrintError("Error: You can only disable relation data collection for dry test runs (the API can't accept the snapshot otherwise)")
			return
		}
	}

	if pidFilename != "" {
		pid := os.Getpid()
		err := ioutil.WriteFile(pidFilename, []byte(strconv.Itoa(pid)), 0644)
		if err != nil {
			logger.PrintError("Could not write pidfile to \"%s\" as requested, exiting.", pidFilename)
			return
		}
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)

	wg := sync.WaitGroup{}

ReadConfigAndRun:
	stop := run(wg, globalCollectionOpts, logger, configFilename)
	if stop == nil {
		return
	}

	// Block here until we get any of the registered signals
	s := <-sigs

	// Stop the scheduled runs
	stop <- true

	if s == syscall.SIGHUP {
		logger.PrintInfo("Reloading configuration...")
		goto ReadConfigAndRun
	}

	signal.Stop(sigs)

	logger.PrintInfo("Exiting...")
	wg.Wait()
}