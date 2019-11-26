package pgprometheus

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/timescale/prometheus-postgresql-adapter/pkg/log"
	"github.com/timescale/prometheus-postgresql-adapter/pkg/util"

	_ "github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/prompb"
)

// Config for the database
type Config struct {
	host                      string
	port                      int
	user                      string
	password                  string
	database                  string
	schema                    string
	sslMode                   string
	table                     string
	copyTable                 string
	maxOpenConns              int
	maxIdleConns              int
	pgPrometheusNormalize     bool
	pgPrometheusLogSamples    bool
	pgPrometheusChunkInterval time.Duration
	useTimescaleDb            bool
	dbConnectRetries          int
	readOnly                  bool
}

// ParseFlags parses the configuration flags specific to PostgreSQL and TimescaleDB
func ParseFlags(cfg *Config) *Config {
	flag.StringVar(&cfg.host, "pg-host", "localhost", "The PostgreSQL host")
	flag.IntVar(&cfg.port, "pg-port", 5432, "The PostgreSQL port")
	flag.StringVar(&cfg.user, "pg-user", "postgres", "The PostgreSQL user")
	flag.StringVar(&cfg.password, "pg-password", "", "The PostgreSQL password")
	flag.StringVar(&cfg.database, "pg-database", "postgres", "The PostgreSQL database")
	flag.StringVar(&cfg.schema, "pg-schema", "", "The PostgreSQL schema")
	flag.StringVar(&cfg.sslMode, "pg-ssl-mode", "disable", "The PostgreSQL connection ssl mode")
	flag.StringVar(&cfg.table, "pg-table", "metrics", "Override prefix for internal tables. It is also a view name used for querying")
	flag.StringVar(&cfg.copyTable, "pg-copy-table", "", "Override default table to COPY data to")
	flag.IntVar(&cfg.maxOpenConns, "pg-max-open-conns", 50, "The max number of open connections to the database")
	flag.IntVar(&cfg.maxIdleConns, "pg-max-idle-conns", 10, "The max number of idle connections to the database")
	flag.BoolVar(&cfg.pgPrometheusNormalize, "pg-prometheus-normalized-schema", true, "Insert metric samples into normalized schema")
	flag.BoolVar(&cfg.pgPrometheusLogSamples, "pg-prometheus-log-samples", false, "Log raw samples to stdout")
	flag.DurationVar(&cfg.pgPrometheusChunkInterval, "pg-prometheus-chunk-interval", time.Hour*12, "The size of a time-partition chunk in TimescaleDB")
	flag.BoolVar(&cfg.useTimescaleDb, "pg-use-timescaledb", true, "Use timescaleDB")
	flag.IntVar(&cfg.dbConnectRetries, "pg-db-connect-retries", 0, "How many times to retry connecting to the database")
	flag.BoolVar(&cfg.readOnly, "pg-read-only", false, "Read-only mode. Don't write to database. Useful when pointing adapter to read replica")
	return cfg
}

// Client sends Prometheus samples to PostgreSQL
type Client struct {
	DB  *sql.DB
	cfg *Config
}

const (
	sqlCreateTmpTable = "CREATE TEMPORARY TABLE IF NOT EXISTS %s_tmp(sample prom_sample) ON COMMIT DELETE ROWS;"
	sqlCopyTable      = "COPY \"%s\" FROM STDIN"
	sqlInsertLabels   = "INSERT INTO %s_labels (metric_name, labels) SELECT tmp.prom_name, tmp.prom_labels FROM (SELECT prom_time(sample), prom_value(sample), prom_name(sample), prom_labels(sample) FROM %s_tmp) tmp LEFT JOIN %s_labels l ON tmp.prom_name=l.metric_name AND tmp.prom_labels=l.labels WHERE l.metric_name IS NULL ON CONFLICT (metric_name, labels) DO NOTHING;"
	sqlInsertValues   = "INSERT INTO %s_values SELECT tmp.prom_time, tmp.prom_value, l.id FROM (SELECT prom_time(sample), prom_value(sample), prom_name(sample), prom_labels(sample) FROM %s_tmp) tmp INNER JOIN %s_labels l on tmp.prom_name=l.metric_name AND  tmp.prom_labels=l.labels;"
)

var (
	createTmpTableStmt *sql.Stmt
)

// NewClient creates a new PostgreSQL client
func NewClient(cfg *Config) *Client {
	connStr := fmt.Sprintf("host=%v port=%v user=%v dbname=%v password='%v' sslmode=%v connect_timeout=10",
		cfg.host, cfg.port, cfg.user, cfg.database, cfg.password, cfg.sslMode)

	wrappedDb, err := util.RetryWithFixedDelay(uint(cfg.dbConnectRetries), time.Second, func() (interface{}, error) {
		return sql.Open("postgres", connStr)
	})

	log.Info("msg", regexp.MustCompile("password='(.+?)'").ReplaceAllLiteralString(connStr, "password='****'"))

	if err != nil {
		log.Error("err", err)
		os.Exit(1)
	}

	db := wrappedDb.(*sql.DB)

	db.SetMaxOpenConns(cfg.maxOpenConns)
	db.SetMaxIdleConns(cfg.maxIdleConns)

	client := &Client{
		DB:  db,
		cfg: cfg,
	}

	if !cfg.readOnly {
		err = client.setupPgPrometheus()

		if err != nil {
			log.Error("err", err)
			os.Exit(1)
		}

		createTmpTableStmt, err = db.Prepare(fmt.Sprintf(sqlCreateTmpTable, cfg.table))
		if err != nil {
			log.Error("msg", "Error on preparing create tmp table statement", "err", err)
			os.Exit(1)
		}
	} else {
		log.Info("msg", "Running in read-only mode. Skipping schema/extension setup (should already be present)")
	}

	return client
}

func (c *Client) setupPgPrometheus() error {
	tx, err := c.DB.Begin()

	if err != nil {
		return err
	}

	defer tx.Rollback()

	_, err = tx.Exec("CREATE EXTENSION IF NOT EXISTS pg_prometheus")

	if err != nil {
		return err
	}

	if c.cfg.useTimescaleDb {
		_, err = tx.Exec("CREATE EXTENSION IF NOT EXISTS timescaledb CASCADE")
	}
	if err != nil {
		log.Info("msg", "Could not enable TimescaleDB extension", "err", err)
	}

	var rows *sql.Rows
	rows, err = tx.Query("SELECT create_prometheus_table($1, normalized_tables => $2, chunk_time_interval => $3,  use_timescaledb=> $4)",
		c.cfg.table, c.cfg.pgPrometheusNormalize, c.cfg.pgPrometheusChunkInterval.String(), c.cfg.useTimescaleDb)

	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			return nil
		}
		return err
	}
	rows.Close()

	err = tx.Commit()

	if err != nil {
		return err
	}

	log.Info("msg", "Initialized pg_prometheus extension")

	return nil
}

func (c *Client) ReadOnly() bool {
	return c.cfg.readOnly
}

func metricString(m model.Metric) string {
	metricName, hasName := m[model.MetricNameLabel]
	numLabels := len(m) - 1
	if !hasName {
		numLabels = len(m)
	}
	labelStrings := make([]string, 0, numLabels)
	for label, value := range m {
		if label != model.MetricNameLabel {
			labelStrings = append(labelStrings, fmt.Sprintf("%s=%q", label, value))
		}
	}

	switch numLabels {
	case 0:
		if hasName {
			return string(metricName)
		}
		return "{}"
	default:
		sort.Strings(labelStrings)
		return fmt.Sprintf("%s{%s}", metricName, strings.Join(labelStrings, ","))
	}
}

// Write implements the Writer interface and writes metric samples to the database
func (c *Client) Write(samples model.Samples) error {
	begin := time.Now()
	tx, err := c.DB.Begin()

	if err != nil {
		log.Error("msg", "Error on Begin when writing samples", "err", err)
		return err
	}

	defer tx.Rollback()

	_, err = tx.Stmt(createTmpTableStmt).Exec()
	if err != nil {
		log.Error("msg", "Error executing create tmp table", "err", err)
		return err
	}

	var copyTable string
	if len(c.cfg.copyTable) > 0 {
		copyTable = c.cfg.copyTable
	} else if c.cfg.pgPrometheusNormalize {
		copyTable = fmt.Sprintf("%s_tmp", c.cfg.table)
	} else {
		copyTable = fmt.Sprintf("%s_samples", c.cfg.table)
	}
	copyStmt, err := tx.Prepare(fmt.Sprintf(sqlCopyTable, copyTable))

	if err != nil {
		log.Error("msg", "Error on COPY prepare", "err", err)
		return err
	}

	for _, sample := range samples {
		milliseconds := sample.Timestamp.UnixNano() / 1000000
		line := fmt.Sprintf("%v %v %v", metricString(sample.Metric), sample.Value, milliseconds)

		if c.cfg.pgPrometheusLogSamples {
			fmt.Println(line)
		}

		_, err = copyStmt.Exec(line)
		if err != nil {
			log.Error("msg", "Error executing COPY statement", "stmt", line, "err", err)
			return err
		}
	}

	_, err = copyStmt.Exec()
	if err != nil {
		log.Error("msg", "Error executing COPY statement", "err", err)
		return err
	}

	if copyTable == fmt.Sprintf("%s_tmp", c.cfg.table) {
		stmtLabels, err := tx.Prepare(fmt.Sprintf(sqlInsertLabels, c.cfg.table, c.cfg.table, c.cfg.table))
		if err != nil {
			log.Error("msg", "Error on preparing labels statement", "err", err)
			return err
		}
		_, err = stmtLabels.Exec()
		if err != nil {
			log.Error("msg", "Error executing labels statement", "err", err)
			return err
		}

		stmtValues, err := tx.Prepare(fmt.Sprintf(sqlInsertValues, c.cfg.table, c.cfg.table, c.cfg.table))
		if err != nil {
			log.Error("msg", "Error on preparing values statement", "err", err)
			return err
		}
		_, err = stmtValues.Exec()
		if err != nil {
			log.Error("msg", "Error executing values statement", "err", err)
			return err
		}

		err = stmtLabels.Close()
		if err != nil {
			log.Error("msg", "Error on closing labels statement", "err", err)
			return err
		}

		err = stmtValues.Close()
		if err != nil {
			log.Error("msg", "Error on closing values statement", "err", err)
			return err
		}
	}

	err = copyStmt.Close()
	if err != nil {
		log.Error("msg", "Error on COPY Close when writing samples", "err", err)
		return err
	}

	err = tx.Commit()

	if err != nil {
		log.Error("msg", "Error on Commit when writing samples", "err", err)
		return err
	}

	duration := time.Since(begin).Seconds()

	log.Debug("msg", "Wrote samples", "count", len(samples), "duration", duration)

	return nil
}

type sampleLabels struct {
	JSON        []byte
	Map         map[string]string
	OrderedKeys []string
}

func createOrderedKeys(m *map[string]string) []string {
	keys := make([]string, 0, len(*m))
	for k := range *m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (c *Client) Close() {
	if c.DB != nil {
		if err := c.DB.Close(); err != nil {
			log.Error("msg", err.Error())
		}
	}
}

func (l *sampleLabels) Scan(value interface{}) error {
	if value == nil {
		l = &sampleLabels{}
		return nil
	}

	switch t := value.(type) {
	case []uint8:
		m := make(map[string]string)
		err := json.Unmarshal(t, &m)

		if err != nil {
			return err
		}

		*l = sampleLabels{
			JSON:        t,
			Map:         m,
			OrderedKeys: createOrderedKeys(&m),
		}
		return nil
	}
	return fmt.Errorf("invalid labels value %s", reflect.TypeOf(value))
}

func (l sampleLabels) String() string {
	return string(l.JSON)
}

func (l sampleLabels) key(extra string) string {
	// 0xff cannot cannot occur in valid UTF-8 sequences, so use it
	// as a separator here.
	separator := "\xff"
	pairs := make([]string, 0, len(l.Map)+1)
	pairs = append(pairs, extra+separator)

	for _, k := range l.OrderedKeys {
		pairs = append(pairs, k+separator+l.Map[k])
	}
	return strings.Join(pairs, separator)
}

func (l *sampleLabels) len() int {
	return len(l.OrderedKeys)
}

// Read implements the Reader interface and reads metrics samples from the database
func (c *Client) Read(req *prompb.ReadRequest) (*prompb.ReadResponse, error) {
	labelsToSeries := map[string]*prompb.TimeSeries{}

	for _, q := range req.Queries {
		command, err := c.buildCommand(q)

		if err != nil {
			return nil, err
		}

		log.Debug("msg", "Executed query", "query", command)

		rows, err := c.DB.Query(command)

		if err != nil {
			return nil, err
		}

		defer rows.Close()

		for rows.Next() {
			var (
				value  float64
				name   string
				labels sampleLabels
				time   time.Time
			)
			err := rows.Scan(&time, &name, &value, &labels)

			if err != nil {
				return nil, err
			}

			key := labels.key(name)
			ts, ok := labelsToSeries[key]

			if !ok {
				labelPairs := make([]prompb.Label, 0, labels.len()+1)
				labelPairs = append(labelPairs, prompb.Label{
					Name:  model.MetricNameLabel,
					Value: name,
				})

				for _, k := range labels.OrderedKeys {
					labelPairs = append(labelPairs, prompb.Label{
						Name:  k,
						Value: labels.Map[k],
					})
				}

				ts = &prompb.TimeSeries{
					Labels:  labelPairs,
					Samples: make([]prompb.Sample, 0, 100),
				}
				labelsToSeries[key] = ts
			}

			ts.Samples = append(ts.Samples, prompb.Sample{
				Timestamp: time.UnixNano() / 1000000,
				Value:     value,
			})
		}

		err = rows.Err()

		if err != nil {
			return nil, err
		}
	}

	resp := prompb.ReadResponse{
		Results: []*prompb.QueryResult{
			{
				Timeseries: make([]*prompb.TimeSeries, 0, len(labelsToSeries)),
			},
		},
	}
	for _, ts := range labelsToSeries {
		resp.Results[0].Timeseries = append(resp.Results[0].Timeseries, ts)
		if c.cfg.pgPrometheusLogSamples {
			log.Debug("timeseries", ts.String())
		}
	}

	log.Debug("msg", "Returned response", "#timeseries", len(labelsToSeries))

	return &resp, nil
}

// HealthCheck implements the healtcheck interface
func (c *Client) HealthCheck() error {
	rows, err := c.DB.Query("SELECT 1")

	if err != nil {
		log.Debug("msg", "Health check error", "err", err)
		return err
	}

	rows.Close()
	return nil
}

func toTimestamp(milliseconds int64) time.Time {
	sec := milliseconds / 1000
	nsec := (milliseconds - (sec * 1000)) * 1000000
	return time.Unix(sec, nsec).UTC()
}

func (c *Client) buildQuery(q *prompb.Query) (string, error) {
	matchers := make([]string, 0, len(q.Matchers))
	labelEqualPredicates := make(map[string]string)

	for _, m := range q.Matchers {
		escapedName := escapeValue(m.Name)
		escapedValue := escapeValue(m.Value)

		if m.Name == model.MetricNameLabel {
			switch m.Type {
			case prompb.LabelMatcher_EQ:
				if len(escapedValue) == 0 {
					matchers = append(matchers, fmt.Sprintf("(name IS NULL OR name = '')"))
				} else {
					matchers = append(matchers, fmt.Sprintf("name = '%s'", escapedValue))
				}
			case prompb.LabelMatcher_NEQ:
				matchers = append(matchers, fmt.Sprintf("name != '%s'", escapedValue))
			case prompb.LabelMatcher_RE:
				matchers = append(matchers, fmt.Sprintf("name ~ '%s'", anchorValue(escapedValue)))
			case prompb.LabelMatcher_NRE:
				matchers = append(matchers, fmt.Sprintf("name !~ '%s'", anchorValue(escapedValue)))
			default:
				return "", fmt.Errorf("unknown metric name match type %v", m.Type)
			}
		} else {
			switch m.Type {
			case prompb.LabelMatcher_EQ:
				if len(escapedValue) == 0 {
					// From the PromQL docs: "Label matchers that match
					// empty label values also select all time series that
					// do not have the specific label set at all."
					matchers = append(matchers, fmt.Sprintf("((labels ? '%s') = false OR (labels->>'%s' = ''))",
						escapedName, escapedName))
				} else {
					labelEqualPredicates[escapedName] = escapedValue
				}
			case prompb.LabelMatcher_NEQ:
				matchers = append(matchers, fmt.Sprintf("labels->>'%s' != '%s'", escapedName, escapedValue))
			case prompb.LabelMatcher_RE:
				matchers = append(matchers, fmt.Sprintf("labels->>'%s' ~ '%s'", escapedName, anchorValue(escapedValue)))
			case prompb.LabelMatcher_NRE:
				matchers = append(matchers, fmt.Sprintf("labels->>'%s' !~ '%s'", escapedName, anchorValue(escapedValue)))
			default:
				return "", fmt.Errorf("unknown match type %v", m.Type)
			}
		}
	}
	equalsPredicate := ""

	if len(labelEqualPredicates) > 0 {
		labelsJSON, err := json.Marshal(labelEqualPredicates)

		if err != nil {
			return "", err
		}
		equalsPredicate = fmt.Sprintf(" AND labels @> '%s'", labelsJSON)
	}

	matchers = append(matchers, fmt.Sprintf("time >= '%v'", toTimestamp(q.StartTimestampMs).Format(time.RFC3339)))
	matchers = append(matchers, fmt.Sprintf("time <= '%v'", toTimestamp(q.EndTimestampMs).Format(time.RFC3339)))

	return fmt.Sprintf("SELECT time, name, value, labels FROM %s WHERE %s %s ORDER BY time",
		c.cfg.table, strings.Join(matchers, " AND "), equalsPredicate), nil
}

func (c *Client) buildCommand(q *prompb.Query) (string, error) {
	return c.buildQuery(q)
}

func escapeValue(str string) string {
	return strings.Replace(str, `'`, `''`, -1)
}

// anchorValue adds anchors to values in regexps since PromQL docs
// states that "Regex-matches are fully anchored."
func anchorValue(str string) string {
	l := len(str)

	if l == 0 || (str[0] == '^' && str[l-1] == '$') {
		return str
	}

	if str[0] == '^' {
		return fmt.Sprintf("%s$", str)
	}

	if str[l-1] == '$' {
		return fmt.Sprintf("^%s", str)
	}

	return fmt.Sprintf("^%s$", str)
}

// Name identifies the client as a PostgreSQL client.
func (c Client) Name() string {
	return "PostgreSQL"
}

// Describe implements prometheus.Collector.
func (c *Client) Describe(ch chan<- *prometheus.Desc) {
}

// Collect implements prometheus.Collector.
func (c *Client) Collect(ch chan<- prometheus.Metric) {
	//ch <- c.ignoredSamples
}
