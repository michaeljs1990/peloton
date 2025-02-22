// Copyright (c) 2019 Uber Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cassandra

import (
	"context"
	"reflect"
	"time"

	"github.com/uber/peloton/pkg/common/backoff"
	pelotoncassandra "github.com/uber/peloton/pkg/storage/cassandra"
	"github.com/uber/peloton/pkg/storage/cassandra/impl"
	"github.com/uber/peloton/pkg/storage/objects/base"
	"github.com/uber/peloton/pkg/storage/orm"

	"github.com/gocql/gocql"
	log "github.com/sirupsen/logrus"
	"github.com/uber-go/tally"
	"go.uber.org/yarpc/yarpcerrors"
)

const (
	_defaultRetryTimeout  = 50 * time.Millisecond
	_defaultRetryAttempts = 5

	useCasWrite = true
)

const (
	// operation tags for metrics
	create  = "create"
	cas     = "cas"
	get     = "get"
	getIter = "get_iter"
	update  = "update"
	del     = "delete"
)

type cassandraConnector struct {
	// implements orm.Connector interface
	orm.Connector
	// Session is the gocql session created for this connector
	Session *gocql.Session
	// scope is the storage scope for metrics
	scope tally.Scope
	// scope is the storage scope for success metrics
	executeSuccessScope tally.Scope
	// scope is the storage scope for failure metrics
	executeFailScope tally.Scope

	// Conf is the Cassandra connector config for this cluster
	Conf *pelotoncassandra.Config
	// retryPolicy defines a DB query retry policy for this connector
	retryPolicy backoff.RetryPolicy
}

// Config is the config for cassandra Store
type Config struct {
	// CassandraConn is the cassandra specific configuration
	CassandraConn *impl.CassandraConn `yaml:"connection"`
	// Store name is the keyspace name in case of Cassandra
	StoreName string `yaml:"store_name"`
}

// NewCassandraConnector initializes a Cassandra Connector
func NewCassandraConnector(
	config *pelotoncassandra.Config, scope tally.Scope) (
	orm.Connector, error) {
	session, err := impl.CreateStoreSession(
		config.CassandraConn, config.StoreName)
	if err != nil {
		return nil, err
	}

	// create a storeScope for the keyspace StoreName
	storeScope := scope.SubScope("cql").Tagged(
		map[string]string{"store": config.StoreName})

	return &cassandraConnector{
		Session: session,
		scope:   storeScope,
		executeSuccessScope: storeScope.Tagged(
			map[string]string{"result": "success"}),
		executeFailScope: storeScope.Tagged(
			map[string]string{"result": "fail"}),
		Conf: config,
		retryPolicy: backoff.NewRetryPolicy(
			_defaultRetryAttempts, _defaultRetryTimeout),
	}, nil
}

// ensure that implementation (cassandraConnector) satisfies the interface
var _ orm.Connector = (*cassandraConnector)(nil)

// getGocqlErrorTag gets a error tag for metrics based on gocql error
// We cannot just use err.Error() as a tag because it contains invalid
// characters like = : etc. which will be rejected by M3
func getGocqlErrorTag(err error) string {
	switch err.(type) {
	case *gocql.RequestErrReadFailure:
		return "read_failure"
	case *gocql.RequestErrWriteFailure:
		return "write_failure"
	case *gocql.RequestErrAlreadyExists:
		return "already_exists"
	case *gocql.RequestErrReadTimeout:
		return "read_timeout"
	case *gocql.RequestErrWriteTimeout:
		return "write_timeout"
	case *gocql.RequestErrUnavailable:
		return "unavailable"
	case *gocql.RequestErrFunctionFailure:
		return "function_failure"
	case *gocql.RequestErrUnprepared:
		return "unprepared"
	default:
		return "unknown"
	}
}

// buildResultRow is used to allocate memory for the row to be populated by
// Cassandra read operation based on what object fields are being read
func buildResultRow(e *base.Definition, columns []string) []interface{} {

	results := make([]interface{}, len(columns))
	timeType := reflect.ValueOf(time.Now())
	gocqlUUIDType := reflect.ValueOf(gocql.UUIDFromTime(time.Now()))

	for i, column := range columns {
		// get the type of the field from the ColumnToType mapping for object
		// That we we can allocate appropriate memory for this field
		typ := e.ColumnToType[column]

		switch typ.Kind() {
		case reflect.String:
			var value *string
			results[i] = &value
		case reflect.Int32, reflect.Uint32, reflect.Int:
			// C* internally uses int and int64
			var value *int
			results[i] = &value
		case reflect.Int64, reflect.Uint64:
			// C* internally uses int and int64
			var value *int64
			results[i] = &value
		case reflect.Bool:
			var value *bool
			results[i] = &value
		case reflect.Slice:
			var value *[]byte
			results[i] = &value
		case timeType.Kind():
			var value *time.Time
			results[i] = &value
		case gocqlUUIDType.Kind():
			var value *gocql.UUID
			results[i] = &value
		default:
			// This should only happen if we start using a new cassandra type
			// without adding to the translation layer
			log.WithFields(log.Fields{"type": typ.Kind(), "column": column}).
				Infof("type not found")
		}
	}

	return results
}

// getRowFromResult translates a row read from Cassandra into a list of
// base.Column to be interpreted by base store client
func getRowFromResult(
	e *base.Definition, columnNames []string, columnVals []interface{},
) []base.Column {

	row := make([]base.Column, 0, len(columnNames))

	for i, columnName := range columnNames {
		// construct a list of column objects from the lists of column names
		// and values that were returned by the cassandra query
		column := base.Column{
			Name: columnName,
		}

		switch rv := columnVals[i].(type) {
		case **int:
			column.Value = *rv
		case **int64:
			column.Value = *rv
		case **string:
			column.Value = *rv
		case **gocql.UUID:
			column.Value = *rv
		case **time.Time:
			column.Value = *rv
		case **bool:
			column.Value = *rv
		case **[]byte:
			column.Value = *rv
		default:
			// This should only happen if we start using a new cassandra type
			// without adding to the translation layer
			log.WithFields(log.Fields{
				"data":   columnVals[i],
				"column": columnName}).Infof("type not found")
		}
		row = append(row, column)
	}
	return row
}

// splitColumnNameValue is used to return list of column names and list of their
// corresponding value. Order is very important in this lists as they will be
// used separately when constructing the CQL query.
func splitColumnNameValue(row []base.Column) (
	colNames []string, colValues []interface{}) {

	// Split row into two lists of column names and column values.
	// So for a location `i` in the list, the colNames[i] and colValues[i] will
	// represent row[i]
	for _, column := range row {
		colNames = append(colNames, column.Name)
		colValues = append(colValues, column.Value)
	}

	return colNames, colValues
}

// Create creates a new row in DB if it already doesn't exist. Uses CAS write.
func (c *cassandraConnector) CreateIfNotExists(
	ctx context.Context,
	e *base.Definition,
	row []base.Column,
) error {
	return c.create(ctx, e, row, useCasWrite)
}

// Create creates a new row in DB.
func (c *cassandraConnector) Create(
	ctx context.Context,
	e *base.Definition,
	row []base.Column,
) error {
	return c.create(ctx, e, row, !useCasWrite)
}

func (c *cassandraConnector) create(
	ctx context.Context,
	e *base.Definition,
	row []base.Column,
	casWrite bool,
) error {
	// split row into a list of names and values to compose query stmt using
	// names and use values in the session query call, so the order needs to be
	// maintained.
	colNames, colValues := splitColumnNameValue(row)

	// Prepare insert statement
	stmt, err := InsertStmt(
		Table(e.Name),
		Columns(colNames),
		Values(colValues),
		IfNotExist(casWrite),
	)
	if err != nil {
		return err
	}

	operation := create
	if casWrite {
		operation = cas
	}

	q := c.Session.Query(stmt, colValues...).WithContext(ctx)

	if casWrite {
		applied, err := q.MapScanCAS(map[string]interface{}{})
		if err != nil {
			sendCounters(c.executeFailScope, e.Name, operation, err)
			return err
		}
		if !applied {
			return yarpcerrors.AlreadyExistsErrorf("item already exists")
		}
	} else {
		if err := q.Exec(); err != nil {
			sendCounters(c.executeFailScope, e.Name, operation, err)
			return err
		}
	}

	sendLatency(c.scope, e.Name, operation, time.Duration(q.Latency()))
	sendCounters(c.executeSuccessScope, e.Name, operation, nil)
	return nil
}

// buildSelectQuery builds a select query using base object and key columns
func (c *cassandraConnector) buildSelectQuery(
	ctx context.Context,
	e *base.Definition,
	keyCols []base.Column,
	colNamesToRead []string,
) (*gocql.Query, error) {

	// split keyCols into a list of names and values to compose query stmt using
	// names and use values in the session query call, so the order needs to be
	// maintained.
	keyColNames, keyColValues := splitColumnNameValue(keyCols)

	// Prepare select statement
	stmt, err := SelectStmt(
		Table(e.Name),
		Columns(colNamesToRead),
		Conditions(keyColNames),
	)
	if err != nil {
		return nil, err
	}

	return c.Session.Query(stmt, keyColValues...).WithContext(ctx), nil
}

// Get fetches a record from DB using primary keys
func (c *cassandraConnector) Get(
	ctx context.Context,
	e *base.Definition,
	keyCols []base.Column,
) ([]base.Column, error) {

	colNamesToRead := e.GetColumnsToRead()

	q, err := c.buildSelectQuery(ctx, e, keyCols, colNamesToRead)
	if err != nil {
		return nil, err
	}

	// build a result row
	result := buildResultRow(e, colNamesToRead)

	if err := q.Scan(result...); err != nil {
		sendCounters(c.executeFailScope, e.Name, get, err)
		return nil, err
	}

	sendLatency(c.scope, e.Name, get, time.Duration(q.Latency()))
	sendCounters(c.executeSuccessScope, e.Name, get, nil)

	// translate the read result into a row ([]base.Column)
	return getRowFromResult(e, colNamesToRead, result), nil
}

// GetAll fetches all rows from DB using partition keys
func (c *cassandraConnector) GetAll(
	ctx context.Context,
	e *base.Definition,
	keyCols []base.Column,
) (rows [][]base.Column, errors error) {
	iter, err := c.GetAllIter(ctx, e, keyCols)
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	for {
		row, errors := iter.Next()
		if errors != nil {
			return nil, errors
		}
		if row != nil {
			rows = append(rows, row)
		} else {
			return rows, nil
		}
	}
}

// GetAllIter gives an iterator to fetch all rows from DB
func (c *cassandraConnector) GetAllIter(
	ctx context.Context,
	e *base.Definition,
	keyCols []base.Column,
) (iter orm.Iterator, err error) {
	colNamesToRead := e.GetColumnsToRead()

	q, err := c.buildSelectQuery(ctx, e, keyCols, colNamesToRead)
	if err != nil {
		return nil, err
	}

	// execute query and get iterator
	cqlIter := q.Iter()
	sendLatency(c.scope, e.Name, getIter, time.Duration(q.Latency()))

	return newIterator(
		e,
		colNamesToRead,
		c.executeSuccessScope,
		c.executeFailScope,
		cqlIter,
	), nil
}

// Delete deletes a record from DB using primary keys
func (c *cassandraConnector) Delete(
	ctx context.Context,
	e *base.Definition,
	keyCols []base.Column,
) error {

	// split keyCols into a list of names and values to compose query stmt using
	// names and use values in the session query call, so the order needs to be
	// maintained.
	keyColNames, keyColValues := splitColumnNameValue(keyCols)

	// Prepare delete statement
	stmt, err := DeleteStmt(
		Table(e.Name),
		Conditions(keyColNames),
	)
	if err != nil {
		return err
	}

	q := c.Session.Query(stmt, keyColValues...).WithContext(ctx)

	if err := q.Exec(); err != nil {
		sendCounters(c.executeFailScope, e.Name, del, err)
		return err
	}

	sendLatency(c.scope, e.Name, del, time.Duration(q.Latency()))
	sendCounters(c.executeSuccessScope, e.Name, del, nil)
	return nil
}

// Update updates an existing row in DB.
func (c *cassandraConnector) Update(
	ctx context.Context,
	e *base.Definition,
	row []base.Column,
	keyCols []base.Column,
) error {

	// split keyCols into a list of names and values to compose query stmt using
	// names and use values in the session query call, so the order needs to be
	// maintained.
	keyColNames, keyColValues := splitColumnNameValue(keyCols)

	// split row into a list of names and values to compose query stmt using
	// names and use values in the session query call, so the order needs to be
	// maintained.
	colNames, colValues := splitColumnNameValue(row)

	// Prepare update statement
	stmt, err := UpdateStmt(
		Table(e.Name),
		Updates(colNames),
		Conditions(keyColNames),
	)

	if err != nil {
		return err
	}

	// list of values to be supplied in the query
	updateVals := append(colValues, keyColValues...)

	q := c.Session.Query(
		stmt, updateVals...).WithContext(ctx)

	if err := q.Exec(); err != nil {
		sendCounters(c.executeFailScope, e.Name, update, err)
		return err
	}

	sendLatency(c.scope, e.Name, update, time.Duration(q.Latency()))
	sendCounters(c.executeSuccessScope, e.Name, update, nil)
	return nil
}

// cassandraIterator implements interface Iterator for Cassandra
type cassandraIterator struct {
	cqlIter        *gocql.Iter
	tableDef       *base.Definition
	colNamesToRead []string
	successScope   tally.Scope
	failScope      tally.Scope
}

// ensure that implementation (cassandraIterator) satisfies the interface
var _ orm.Iterator = (*cassandraIterator)(nil)

func newIterator(
	e *base.Definition,
	cols []string,
	successScope tally.Scope,
	failScope tally.Scope,
	cqlIter *gocql.Iter,
) *cassandraIterator {
	return &cassandraIterator{
		cqlIter:        cqlIter,
		tableDef:       e,
		successScope:   successScope,
		failScope:      failScope,
		colNamesToRead: cols,
	}
}

func (iter *cassandraIterator) Close() {
	iter.cqlIter.Close()
}

func (iter *cassandraIterator) Next() ([]base.Column, error) {
	result := buildResultRow(iter.tableDef, iter.colNamesToRead)
	if iter.cqlIter.Scan(result...) {
		row := getRowFromResult(iter.tableDef, iter.colNamesToRead, result)
		return row, nil
	}
	// Either end-of-results or error
	if errors := iter.cqlIter.Close(); errors != nil {
		sendCounters(iter.failScope, iter.tableDef.Name, getIter, errors)
		return nil, errors
	}
	sendCounters(iter.successScope, iter.tableDef.Name, getIter, nil)
	return nil, nil
}

// helper function to record call latency metric
func sendLatency(
	scope tally.Scope,
	table, operation string,
	d time.Duration,
) {
	s := scope.Tagged(map[string]string{
		"table":     table,
		"operation": operation,
	})
	s.Timer("execute_latency").Record(d)
}

// helper function to record cql query success/failure metrics
func sendCounters(
	scope tally.Scope,
	table, operation string,
	err error,
) {
	errMsg := "none"
	if err != nil {
		errMsg = getGocqlErrorTag(err)
	}
	s := scope.Tagged(map[string]string{
		"table":     table,
		"operation": operation,
		"error":     errMsg,
	})
	s.Counter("execute").Inc(1)
}
