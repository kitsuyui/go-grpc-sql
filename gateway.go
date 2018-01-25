package grpcsql

import (
	"database/sql/driver"
	"fmt"
	"io"
	"sync"

	"github.com/CanonicalLtd/go-grpc-sql/internal/protocol"
	"github.com/CanonicalLtd/go-sqlite3"
	"github.com/golang/protobuf/proto"
	"github.com/pkg/errors"
)

// Gateway mapping gRPC requests to SQL queries.
type Gateway struct {
	driver driver.Driver // Underlying SQL driver.
}

// NewGateway creates a new gRPC gateway executing requests against the given
// SQL driver.
func NewGateway(drv driver.Driver) *Gateway {
	return &Gateway{
		driver: drv,
	}
}

// Conn creates a new database connection using the underlying driver, and
// start accepting requests for it.
func (s *Gateway) Conn(stream protocol.SQL_ConnServer) error {
	var conn *gatewayConn
	for {
		request, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return errors.Wrapf(err, "failed to receive request")
		}

		if conn == nil {
			conn = &gatewayConn{
				driver: s.driver,
			}
			defer conn.Close()
		}

		response, err := conn.handle(request)
		if err != nil {
			// TODO: add support for more driver-specific errors.
			switch err := err.(type) {
			case sqlite3.Error:
				response = protocol.NewResponseSQLiteError(
					err.Code, err.ExtendedCode, err.Error())
			case sqlite3.ErrNo:
				response = protocol.NewResponseSQLiteError(
					err, 0, err.Error())
			default:
				return errors.Wrapf(err, "failed to handle %s request", request.Code)
			}
		}

		if err := stream.Send(response); err != nil {
			return errors.Wrapf(err, "failed to send response")
		}
	}
}

// Track a single driver connection
type gatewayConn struct {
	driver     driver.Driver
	driverConn driver.Conn
	stmts      map[int64]driver.Stmt
	txs        map[int64]driver.Tx
	rows       map[int64]driver.Rows
	serial     int64
	mu         sync.Mutex
}

// Handle a single gRPC request for this connection.
func (c *gatewayConn) handle(request *protocol.Request) (*protocol.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var message proto.Message

	switch request.Code {
	case protocol.RequestCode_OPEN:
		message = &protocol.RequestOpen{}
	case protocol.RequestCode_PREPARE:
		message = &protocol.RequestPrepare{}
	case protocol.RequestCode_EXEC:
		message = &protocol.RequestExec{}
	case protocol.RequestCode_QUERY:
		message = &protocol.RequestQuery{}
	case protocol.RequestCode_NEXT:
		message = &protocol.RequestNext{}
	case protocol.RequestCode_COLUMN_TYPE_SCAN_TYPE:
		message = &protocol.RequestColumnTypeScanType{}
	case protocol.RequestCode_COLUMN_TYPE_DATABASE_TYPE_NAME:
		message = &protocol.RequestColumnTypeDatabaseTypeName{}
	case protocol.RequestCode_ROWS_CLOSE:
		message = &protocol.RequestRowsClose{}
	case protocol.RequestCode_STMT_CLOSE:
		message = &protocol.RequestStmtClose{}
	case protocol.RequestCode_BEGIN:
		message = &protocol.RequestBegin{}
	case protocol.RequestCode_COMMIT:
		message = &protocol.RequestCommit{}
	case protocol.RequestCode_ROLLBACK:
		message = &protocol.RequestRollback{}
	case protocol.RequestCode_CLOSE:
		message = &protocol.RequestClose{}
	case protocol.RequestCode_CONN_EXEC:
		message = &protocol.RequestConnExec{}
	default:
		return nil, fmt.Errorf("invalid request code %d", request.Code)
	}

	if err := proto.Unmarshal(request.Data, message); err != nil {
		return nil, errors.Wrapf(err, "request parse error")
	}

	// The very first request must be an OPEN one.
	if c.driverConn == nil && request.Code != protocol.RequestCode_OPEN {
		return nil, fmt.Errorf("expected OPEN request, got %s", request.Code)
	}

	switch r := message.(type) {
	case *protocol.RequestOpen:
		return c.handleOpen(r)
	case *protocol.RequestPrepare:
		return c.handlePrepare(r)
	case *protocol.RequestExec:
		return c.handleExec(r)
	case *protocol.RequestQuery:
		return c.handleQuery(r)
	case *protocol.RequestNext:
		return c.handleNext(r)
	case *protocol.RequestColumnTypeScanType:
		return c.handleColumnTypeScanType(r)
	case *protocol.RequestColumnTypeDatabaseTypeName:
		return c.handleColumnTypeDatabaseTypeName(r)
	case *protocol.RequestRowsClose:
		return c.handleRowsClose(r)
	case *protocol.RequestStmtClose:
		return c.handleStmtClose(r)
	case *protocol.RequestBegin:
		return c.handleBegin(r)
	case *protocol.RequestCommit:
		return c.handleCommit(r)
	case *protocol.RequestRollback:
		return c.handleRollback(r)
	case *protocol.RequestClose:
		return c.handleClose(r)
	case *protocol.RequestConnExec:
		return c.handleConnExec(r)
	default:
		panic("unhandled request payload type")
	}
}

// Close the underlying driver connection, if any.
func (c *gatewayConn) Close() error {
	if c.driverConn != nil {
		return c.driverConn.Close()
	}
	return nil
}

// Handle a request of type OPEN.
func (c *gatewayConn) handleOpen(request *protocol.RequestOpen) (*protocol.Response, error) {
	driverConn, err := c.driver.Open(request.Name)
	if err != nil {
		return nil, fmt.Errorf("could not open driver connection: %v", err)
	}

	c.driverConn = driverConn
	c.stmts = make(map[int64]driver.Stmt)
	c.txs = make(map[int64]driver.Tx)
	c.rows = make(map[int64]driver.Rows)

	response := protocol.NewResponseOpen()
	return response, nil
}

// Handle a request of type PREPARE.
func (c *gatewayConn) handlePrepare(request *protocol.RequestPrepare) (*protocol.Response, error) {
	driverStmt, err := c.driverConn.Prepare(request.Query)
	if err != nil {
		return nil, err
	}
	c.serial++
	c.stmts[c.serial] = driverStmt
	return protocol.NewResponsePrepare(c.serial, driverStmt.NumInput()), nil
}

// Handle a request of type EXEC.
func (c *gatewayConn) handleExec(request *protocol.RequestExec) (*protocol.Response, error) {
	driverStmt, ok := c.stmts[request.Id]
	if !ok {
		return nil, fmt.Errorf("no prepared statement with ID %d", request.Id)
	}

	args, err := protocol.ToDriverValues(request.Args)
	if err != nil {
		return nil, err
	}

	result, err := driverStmt.Exec(args)
	if err != nil {
		return nil, err
	}

	lastInsertID, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}

	response := protocol.NewResponseExec(lastInsertID, rowsAffected)

	return response, nil
}

// Handle a request of type QUERY.
func (c *gatewayConn) handleQuery(request *protocol.RequestQuery) (*protocol.Response, error) {
	driverStmt, ok := c.stmts[request.Id]
	if !ok {
		return nil, fmt.Errorf("no prepared statement with ID %d", request.Id)
	}

	args, err := protocol.ToDriverValues(request.Args)
	if err != nil {
		return nil, err
	}

	driverRows, err := driverStmt.Query(args)
	if err != nil {
		return nil, err
	}

	c.serial++
	c.rows[c.serial] = driverRows

	return protocol.NewResponseQuery(c.serial, driverRows.Columns()), nil
}

// Handle a request of type NEXT.
func (c *gatewayConn) handleNext(request *protocol.RequestNext) (*protocol.Response, error) {
	driverRows, ok := c.rows[request.Id]
	if !ok {
		return nil, fmt.Errorf("no rows with ID %d", request.Id)
	}

	dest := make([]driver.Value, int(request.Len))
	err := driverRows.Next(dest)
	if err == io.EOF {
		return protocol.NewResponseNext(true, nil), nil
	}

	if err != nil {
		return nil, err
	}

	values, err := protocol.FromDriverValues(dest)
	if err != nil {
		return nil, err
	}

	return protocol.NewResponseNext(false, values), nil
}

// Handle a request of type COLUMN_TYPE_SCAN_TYPE.
func (c *gatewayConn) handleColumnTypeScanType(request *protocol.RequestColumnTypeScanType) (*protocol.Response, error) {
	driverRows, ok := c.rows[request.Id]
	if !ok {
		return nil, fmt.Errorf("no rows with ID %d", request.Id)
	}

	code := protocol.ValueCode_BYTES
	typeScanner, ok := driverRows.(driver.RowsColumnTypeScanType)
	if ok {
		typ := typeScanner.ColumnTypeScanType(int(request.Column))
		code = protocol.ToValueCode(typ)
	}

	return protocol.NewResponseColumnTypeScanType(code), nil
}

// Handle a request of type COLUMN_TYPE_DATABASE_TYPE_NAME.
func (c *gatewayConn) handleColumnTypeDatabaseTypeName(request *protocol.RequestColumnTypeDatabaseTypeName) (*protocol.Response, error) {
	driverRows, ok := c.rows[request.Id]
	if !ok {
		return nil, fmt.Errorf("no rows with ID %d", request.Id)
	}

	name := ""
	nameScanner, ok := driverRows.(driver.RowsColumnTypeDatabaseTypeName)
	if ok {
		name = nameScanner.ColumnTypeDatabaseTypeName(int(request.Column))
	}

	return protocol.NewResponseColumnTypeDatabaseTypeName(name), nil
}

// Handle a request of type ROWS_CLOSE.
func (c *gatewayConn) handleRowsClose(request *protocol.RequestRowsClose) (*protocol.Response, error) {
	driverRows, ok := c.rows[request.Id]
	if !ok {
		return nil, fmt.Errorf("no rows with ID %d", request.Id)
	}
	delete(c.rows, request.Id)

	if err := driverRows.Close(); err != nil {
		return nil, err
	}

	response := protocol.NewResponseRowsClose()
	return response, nil
}

// Handle a request of type STMT_CLOSE.
func (c *gatewayConn) handleStmtClose(request *protocol.RequestStmtClose) (*protocol.Response, error) {
	driverStmt, ok := c.stmts[request.Id]
	if !ok {
		return nil, fmt.Errorf("no prepared statement with ID %d", request.Id)
	}
	delete(c.stmts, request.Id)

	if err := driverStmt.Close(); err != nil {
		return nil, err
	}

	response := protocol.NewResponseStmtClose()
	return response, nil
}

// Handle a request of type BEGIN.
func (c *gatewayConn) handleBegin(request *protocol.RequestBegin) (*protocol.Response, error) {
	driverTx, err := c.driverConn.Begin()
	if err != nil {
		return nil, err
	}
	c.serial++
	c.txs[c.serial] = driverTx
	return protocol.NewResponseBegin(c.serial), nil
}

// Handle a request of type COMMIT.
func (c *gatewayConn) handleCommit(request *protocol.RequestCommit) (*protocol.Response, error) {
	driverTx, ok := c.txs[request.Id]
	if !ok {
		return nil, fmt.Errorf("no transaction with ID %d", request.Id)
	}
	delete(c.txs, request.Id)

	if err := driverTx.Commit(); err != nil {
		return nil, err
	}

	response := protocol.NewResponseCommit()
	return response, nil
}

// Handle a request of type ROLLBACK.
func (c *gatewayConn) handleRollback(request *protocol.RequestRollback) (*protocol.Response, error) {
	driverTx, ok := c.txs[request.Id]
	if !ok {
		return nil, fmt.Errorf("no transaction with ID %d", request.Id)
	}
	delete(c.txs, request.Id)

	if err := driverTx.Rollback(); err != nil {
		return nil, err
	}

	response := protocol.NewResponseRollback()
	return response, nil
}

// Handle a request of type CLOSE.
func (c *gatewayConn) handleClose(request *protocol.RequestClose) (*protocol.Response, error) {
	if err := c.driverConn.Close(); err != nil {
		return nil, err
	}

	response := protocol.NewResponseClose()
	return response, nil
}

// Handle a request of type CONN_EXEC.
func (c *gatewayConn) handleConnExec(request *protocol.RequestConnExec) (*protocol.Response, error) {
	args, err := protocol.ToDriverValues(request.Args)
	if err != nil {
		return nil, err
	}

	execer, ok := c.driverConn.(driver.Execer)
	if !ok {
		return nil, fmt.Errorf("backend driver does not implement driver.Execer")
	}
	result, err := execer.Exec(request.Query, args)
	if err != nil {
		return nil, err
	}

	lastInsertID, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}

	response := protocol.NewResponseExec(lastInsertID, rowsAffected)

	return response, nil
}
