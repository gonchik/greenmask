package restorers

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"

	"github.com/greenmaskio/greenmask/internal/db/postgres/pgrestore"
	"github.com/greenmaskio/greenmask/internal/db/postgres/toc"
	"github.com/greenmaskio/greenmask/internal/utils/testutils"
)

const (
	migrationUp = `
CREATE USER non_super_user PASSWORD '1234' NOINHERIT;
GRANT testuser TO non_super_user;
GRANT SELECT, INSERT ON ALL TABLES IN SCHEMA public TO non_super_user;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT ON TABLES TO non_super_user;
GRANT INSERT ON ALL TABLES IN SCHEMA public TO non_super_user;

-- Create the 'users' table
CREATE TABLE users (
    id SERIAL PRIMARY KEY,
    name TEXT NOT NULL,
    email TEXT UNIQUE NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- Create the 'orders' table
CREATE TABLE orders (
    id SERIAL PRIMARY KEY,
    user_id INT NOT NULL,
    order_amount NUMERIC(10, 2) NOT NULL,
	raise_error TEXT,
    order_date DATE DEFAULT CURRENT_DATE,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT fk_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
);

-- Trigger function to ensure 'order_date' is always set
CREATE OR REPLACE FUNCTION set_order_date()
RETURNS TRIGGER AS $$
BEGIN
	If NEW.raise_error != '' THEN
		RAISE EXCEPTION '%', NEW.raise_error;
	END IF;
    IF NEW.order_date IS NULL THEN
        NEW.order_date = CURRENT_DATE;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Trigger for 'orders' table
CREATE TRIGGER trg_set_order_date
BEFORE INSERT ON orders
FOR EACH ROW
EXECUTE FUNCTION set_order_date();

-- Insert sample data into 'users'
INSERT INTO users (name, email) VALUES
('Alice', 'alice@example.com'),
('Bob', 'bob@example.com');

-- Insert sample data into 'orders'
INSERT INTO orders (user_id, order_amount) VALUES
(1, 100.50),
(2, 200.75);
`
	migrationDown = `
REVOKE ALL ON SCHEMA public FROM non_super_user;
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM non_super_user;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM non_super_user;
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA public FROM non_super_user;
REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA public FROM non_super_user;
ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE SELECT, INSERT ON TABLES FROM non_super_user;
REVOKE USAGE ON SCHEMA public FROM non_super_user;
REVOKE testuser FROM non_super_user;
DROP USER non_super_user;
DROP TRIGGER IF EXISTS trg_set_order_date ON orders;
DROP FUNCTION IF EXISTS set_order_date;
DROP TABLE IF EXISTS orders;
DROP TABLE IF EXISTS users;
`
)

type readCloserMock struct {
	*bytes.Buffer
}

func (r *readCloserMock) Close() error {
	return nil
}

type restoresSuite struct {
	testutils.PgContainerSuite
	nonSuperUserPassword string
	nonSuperUser         string
}

func (s *restoresSuite) SetupSuite() {
	s.SetMigrationUp(migrationUp).
		SetMigrationDown(migrationDown).
		SetupSuite()
	s.nonSuperUser = "non_super_user"
	s.nonSuperUserPassword = "1234"
}

func (s *restoresSuite) Test_restoreBase_DebugInfo() {
	nsp := "public"
	tag := "orders"
	rb := newRestoreBase(&toc.Entry{
		Namespace: &nsp,
		Tag:       &tag,
	}, nil, nil)
	s.Equal("table public.orders", rb.DebugInfo())
}

func (s *restoresSuite) Test_restoreBase_setSessionReplicationRole() {
	userName := s.GetSuperUser()
	opt := &pgrestore.DataSectionSettings{
		UseSessionReplicationRoleReplica: true,
		SuperUser:                        userName,
	}

	rb := newRestoreBase(nil, nil, opt)
	cxt := context.Background()
	conn, err := s.GetConnectionWithUser(cxt, s.nonSuperUser, s.nonSuperUserPassword)
	s.Require().NoError(err)
	defer conn.Close(cxt) // nolint: errcheck
	s.Require().NoError(err)
	tx, err := conn.Begin(cxt)
	s.Require().NoError(err)
	s.Require().NoError(rb.setSessionReplicationRole(cxt, tx))

	expectedUser := s.nonSuperUser
	expectedReplicaRole := "replica"

	var actualUser string
	r := tx.QueryRow(cxt, "SELECT current_user")
	err = r.Scan(&actualUser)
	s.Require().NoError(err)
	s.Assert().Equal(expectedUser, actualUser)

	var actualReplicaRole string
	r = tx.QueryRow(cxt, "SHOW session_replication_role")
	err = r.Scan(&actualReplicaRole)
	s.Require().NoError(err)
	s.Assert().Equal(expectedReplicaRole, actualReplicaRole)

	s.NoError(tx.Rollback(cxt))
}

func (s *restoresSuite) Test_restoreBase_resetSessionReplicationRole() {
	userName := s.GetSuperUser()
	opt := &pgrestore.DataSectionSettings{
		UseSessionReplicationRoleReplica: true,
		SuperUser:                        userName,
	}

	rb := newRestoreBase(nil, nil, opt)
	cxt := context.Background()
	conn, err := s.GetConnectionWithUser(cxt, s.nonSuperUser, s.nonSuperUserPassword)
	s.Require().NoError(err)
	defer conn.Close(cxt) // nolint: errcheck
	s.Require().NoError(err)
	tx, err := conn.Begin(cxt)
	s.Require().NoError(err)

	_, err = tx.Exec(cxt, "SET ROLE "+s.GetSuperUser())
	s.Require().NoError(err)
	_, err = tx.Exec(cxt, "SET session_replication_role = 'replica'")
	s.Require().NoError(err)

	err = rb.setSessionReplicationRole(cxt, tx)
	s.Require().NoError(err)

	expectedUser := s.nonSuperUser
	expectedReplicaRole := "replica"

	var actualUser string
	r := tx.QueryRow(cxt, "SELECT current_user")
	err = r.Scan(&actualUser)
	s.Require().NoError(err)
	s.Assert().Equal(expectedUser, actualUser)

	var actualReplicaRole string
	r = tx.QueryRow(cxt, "SHOW session_replication_role")
	err = r.Scan(&actualReplicaRole)
	s.Require().NoError(err)
	s.Assert().Equal(expectedReplicaRole, actualReplicaRole)

	s.NoError(tx.Rollback(cxt))
}

func (s *restoresSuite) Test_restoreBase_enableTriggers() {
	schemaName := "public"
	tableName := "orders"
	opt := &pgrestore.DataSectionSettings{
		DisableTriggers: true,
		SuperUser:       s.GetSuperUser(),
	}
	rb := newRestoreBase(&toc.Entry{
		Namespace: &schemaName,
		Tag:       &tableName,
	}, nil, opt)
	ctx := context.Background()
	conn, err := s.GetConnectionWithUser(ctx, s.nonSuperUser, s.nonSuperUserPassword)
	s.Require().NoError(err)
	defer conn.Close(ctx) // nolint: errcheck
	s.Require().NoError(err)
	tx, err := conn.Begin(ctx)
	s.Require().NoError(err)
	err = rb.disableTriggers(ctx, tx)
	s.Require().NoError(err)

	expectedUser := s.nonSuperUser
	var actualUser string
	r := tx.QueryRow(ctx, "SELECT current_user")
	err = r.Scan(&actualUser)
	s.Require().NoError(err)
	s.Assert().Equal(expectedUser, actualUser)

	// tgenabled value:
	// O = trigger fires in “origin” and “local” modes, D = trigger is disabled,
	// R = trigger fires in “replica” mode, A = trigger fires always
	checkDisabledTriggerSql := `
SELECT tgname AS trigger_name,
       tgenabled
FROM pg_trigger t
         JOIN pg_class c ON t.tgrelid = c.oid
		 JOIN pg_namespace n ON c.relnamespace = n.oid
WHERE n.nspname = $1 AND c.relname = $2
	AND t.tgname = ANY($3);
`
	rows, err := conn.Query(
		ctx, checkDisabledTriggerSql, schemaName, tableName, []string{"trg_set_order_date"},
	)
	s.Require().NoError(err)
	defer rows.Close()

	type triggerStatus struct {
		triggerName string
		tgenabled   rune
	}
	var triggers []triggerStatus
	for rows.Next() {
		var t triggerStatus
		err = rows.Scan(&t.triggerName, &t.tgenabled)
		s.Require().NoError(err)
		triggers = append(triggers, t)
	}

	expectedTriggerStatus := []triggerStatus{
		{triggerName: "trg_set_order_date", tgenabled: 'D'},
	}

	s.Require().Len(triggers, len(expectedTriggerStatus))
	for i, expected := range expectedTriggerStatus {
		s.Assert().Equal(expected.triggerName, triggers[i].triggerName)
		s.Assert().Equal(expected.tgenabled, triggers[i].tgenabled)
	}

	s.NoError(tx.Rollback(ctx))
}

func (s *restoresSuite) Test_restoreBase_disableTriggers() {
	cxt := context.Background()
	schemaName := "public"
	tableName := "orders"

	suConn, err := s.GetConnection(cxt)
	s.Require().NoError(err)
	defer suConn.Close(cxt) // nolint: errcheck
	s.Require().NoError(err)
	_, err = suConn.Exec(cxt, "ALTER TABLE public.orders DISABLE TRIGGER ALL")
	s.Require().NoError(err)

	opt := &pgrestore.DataSectionSettings{
		DisableTriggers: true,
		SuperUser:       s.GetSuperUser(),
	}
	rb := newRestoreBase(&toc.Entry{
		Namespace: &schemaName,
		Tag:       &tableName,
	}, nil, opt)
	conn, err := s.GetConnectionWithUser(cxt, s.nonSuperUser, s.nonSuperUserPassword)
	s.Require().NoError(err)
	defer conn.Close(cxt) // nolint: errcheck
	s.Require().NoError(err)
	tx, err := conn.Begin(cxt)
	s.Require().NoError(err)
	err = rb.enableTriggers(cxt, tx)
	s.Require().NoError(err)

	expectedUser := s.nonSuperUser
	var actualUser string
	r := tx.QueryRow(cxt, "SELECT current_user")
	err = r.Scan(&actualUser)
	s.Require().NoError(err)
	s.Assert().Equal(expectedUser, actualUser)

	// tgenabled value:
	// O = trigger fires in “origin” and “local” modes, D = trigger is disabled,
	// R = trigger fires in “replica” mode, A = trigger fires always
	checkDisabledTriggerSql := `
SELECT tgname AS trigger_name,
       tgenabled
FROM pg_trigger t
         JOIN pg_class c ON t.tgrelid = c.oid
		 JOIN pg_namespace n ON c.relnamespace = n.oid
WHERE n.nspname = $1 AND c.relname = $2
	AND t.tgname = ANY($3);
`
	rows, err := conn.Query(
		cxt, checkDisabledTriggerSql, schemaName, tableName, []string{"trg_set_order_date"},
	)
	s.Require().NoError(err)
	defer rows.Close()

	type triggerStatus struct {
		triggerName string
		tgenabled   rune
	}
	var triggers []triggerStatus
	for rows.Next() {
		var t triggerStatus
		err = rows.Scan(&t.triggerName, &t.tgenabled)
		s.Require().NoError(err)
		triggers = append(triggers, t)
	}

	expectedTriggerStatus := []triggerStatus{
		{triggerName: "trg_set_order_date", tgenabled: 'O'},
	}

	s.Require().Len(triggers, len(expectedTriggerStatus))
	for i, expected := range expectedTriggerStatus {
		s.Assert().Equal(expected.triggerName, triggers[i].triggerName)
		s.Assert().Equal(expected.tgenabled, triggers[i].tgenabled)
	}

	s.NoError(tx.Rollback(cxt))
}

func (s *restoresSuite) Test_restoreBase_setSuperUser() {
	cxt := context.Background()
	conn, err := s.GetConnectionWithUser(cxt, s.nonSuperUser, s.nonSuperUserPassword)
	s.Require().NoError(err)
	defer conn.Close(cxt)
	s.Require().NoError(err)
	tx, err := conn.Begin(cxt)
	s.Require().NoError(err)
	defer tx.Rollback(cxt) // nolint: errcheck
	rb := newRestoreBase(nil, nil, &pgrestore.DataSectionSettings{
		SuperUser: s.GetSuperUser(),
	})
	err = rb.setSuperUser(cxt, tx)
	s.Require().NoError(err)

	expectedUser := s.GetSuperUser()
	var actualUser string
	r := conn.QueryRow(cxt, "SELECT current_user")
	err = r.Scan(&actualUser)
	s.Require().NoError(err)
	s.Assert().Equal(expectedUser, actualUser)
}

func (s *restoresSuite) Test_restoreBase_resetSuperUser() {
	cxt := context.Background()
	conn, err := s.GetConnectionWithUser(cxt, s.nonSuperUser, s.nonSuperUserPassword)
	s.Require().NoError(err)
	defer conn.Close(cxt)
	s.Require().NoError(err)
	tx, err := conn.Begin(cxt)
	s.Require().NoError(err)
	defer tx.Rollback(cxt) // nolint: errcheck

	_, err = tx.Exec(cxt, fmt.Sprintf("SET ROLE %s", s.GetSuperUser()))
	s.Require().NoError(err)

	rb := newRestoreBase(nil, nil, &pgrestore.DataSectionSettings{
		SuperUser: s.GetSuperUser(),
	})
	err = rb.resetSuperUser(cxt, tx)
	s.Require().NoError(err)

	expectedUser := s.nonSuperUser
	var actualUser string
	r := conn.QueryRow(cxt, "SELECT current_user")
	err = r.Scan(&actualUser)
	s.Require().NoError(err)
	s.Assert().Equal(expectedUser, actualUser)
}

func (s *restoresSuite) Test_restoreBase_setupTx() {
	// Test triggers enabled
	// Test session replication role enabled
	schemaName := "public"
	tableName := "orders"
	opt := &pgrestore.DataSectionSettings{
		DisableTriggers:                  true,
		UseSessionReplicationRoleReplica: true,
		SuperUser:                        s.GetSuperUser(),
	}
	rb := newRestoreBase(&toc.Entry{
		Namespace: &schemaName,
		Tag:       &tableName,
	}, nil, opt)
	cxt := context.Background()
	conn, err := s.GetConnectionWithUser(cxt, s.nonSuperUser, s.nonSuperUserPassword)
	s.Require().NoError(err)
	defer conn.Close(cxt)
	s.Require().NoError(err)
	tx, err := conn.Begin(cxt)
	s.Require().NoError(err)
	err = rb.setupTx(cxt, tx)
	s.Require().NoError(err)

	// tgenabled value:
	// O = trigger fires in “origin” and “local” modes, D = trigger is disabled,
	// R = trigger fires in “replica” mode, A = trigger fires always
	checkDisabledTriggerSql := `
SELECT tgname AS trigger_name,
       tgenabled
FROM pg_trigger t
         JOIN pg_class c ON t.tgrelid = c.oid
		 JOIN pg_namespace n ON c.relnamespace = n.oid
WHERE n.nspname = $1 AND c.relname = $2
	AND t.tgname = ANY($3);
`
	rows, err := conn.Query(
		cxt, checkDisabledTriggerSql, schemaName, tableName, []string{"trg_set_order_date"},
	)
	s.Require().NoError(err)
	defer rows.Close()

	type triggerStatus struct {
		triggerName string
		tgenabled   rune
	}
	var triggers []triggerStatus
	for rows.Next() {
		var t triggerStatus
		err = rows.Scan(&t.triggerName, &t.tgenabled)
		s.Require().NoError(err)
		triggers = append(triggers, t)
	}

	expectedTriggerStatus := []triggerStatus{
		{triggerName: "trg_set_order_date", tgenabled: 'D'},
	}

	s.Require().Len(triggers, len(expectedTriggerStatus))
	for i, expected := range expectedTriggerStatus {
		s.Assert().Equal(expected.triggerName, triggers[i].triggerName)
		s.Assert().Equal(expected.tgenabled, triggers[i].tgenabled)
	}

	expectedReplicaRole := "replica"
	actualReplicaRole := ""
	r := tx.QueryRow(cxt, "SHOW session_replication_role")
	err = r.Scan(&actualReplicaRole)
	s.Require().NoError(err)
	s.Assert().Equal(expectedReplicaRole, actualReplicaRole)

	s.NoError(tx.Rollback(cxt))
}

func (s *restoresSuite) Test_restoreBase_resetTx() {
	// Test triggers enabled
	// Test session replication role enabled
	schemaName := "public"
	tableName := "orders"
	opt := &pgrestore.DataSectionSettings{
		DisableTriggers:                  true,
		UseSessionReplicationRoleReplica: true,
		SuperUser:                        s.GetSuperUser(),
	}
	rb := newRestoreBase(&toc.Entry{
		Namespace: &schemaName,
		Tag:       &tableName,
	}, nil, opt)
	cxt := context.Background()
	conn, err := s.GetConnectionWithUser(cxt, s.nonSuperUser, s.nonSuperUserPassword)
	s.Require().NoError(err)
	defer conn.Close(cxt)
	s.Require().NoError(err)
	tx, err := conn.Begin(cxt)
	s.Require().NoError(err)

	_, err = tx.Exec(cxt, "SET ROLE "+s.GetSuperUser())
	s.Require().NoError(err)
	_, err = tx.Exec(cxt, "ALTER TABLE public.orders DISABLE TRIGGER ALL")
	s.Require().NoError(err)
	_, err = tx.Exec(cxt, "SET session_replication_role = 'replica'")
	s.Require().NoError(err)

	err = rb.resetTx(cxt, tx)
	s.Require().NoError(err)

	// tgenabled value:
	// O = trigger fires in “origin” and “local” modes, D = trigger is disabled,
	// R = trigger fires in “replica” mode, A = trigger fires always
	checkDisabledTriggerSql := `
SELECT tgname AS trigger_name,
       tgenabled
FROM pg_trigger t
         JOIN pg_class c ON t.tgrelid = c.oid
		 JOIN pg_namespace n ON c.relnamespace = n.oid
WHERE n.nspname = $1 AND c.relname = $2
	AND t.tgname = ANY($3);
`
	rows, err := conn.Query(
		cxt, checkDisabledTriggerSql, schemaName, tableName, []string{"trg_set_order_date"},
	)
	s.Require().NoError(err)
	defer rows.Close()

	type triggerStatus struct {
		triggerName string
		tgenabled   rune
	}
	var triggers []triggerStatus
	for rows.Next() {
		var t triggerStatus
		err = rows.Scan(&t.triggerName, &t.tgenabled)
		s.Require().NoError(err)
		triggers = append(triggers, t)
	}

	expectedTriggerStatus := []triggerStatus{
		{triggerName: "trg_set_order_date", tgenabled: 'O'},
	}

	s.Require().Len(triggers, len(expectedTriggerStatus))
	for i, expected := range expectedTriggerStatus {
		s.Assert().Equal(expected.triggerName, triggers[i].triggerName)
		s.Assert().Equal(expected.tgenabled, triggers[i].tgenabled)
	}

	expectedReplicaRole := "origin"
	actualReplicaRole := ""
	r := tx.QueryRow(cxt, "SHOW session_replication_role")
	err = r.Scan(&actualReplicaRole)
	s.Require().NoError(err)
	s.Assert().Equal(expectedReplicaRole, actualReplicaRole)

	s.NoError(tx.Rollback(cxt))
}

func (s *restoresSuite) Test_restoreBase_getObject() {
	schemaName := "public"
	tableName := "orders"
	fileName := "test.tar.gz"

	data := `20383   24ca7574-0adb-4b17-8777-93f5589dbea2    2017-12-13 13:46:49.39
20384   d0d4a55c-7752-453e-8334-772a889fb917    2017-12-13 13:46:49.453
20385   ac8617aa-5a2d-4bb8-a9a5-ed879a4b33cd    2017-12-13 13:46:49.5
`
	buf := new(bytes.Buffer)
	gzData := gzip.NewWriter(buf)
	_, err := gzData.Write([]byte(data))
	s.Require().NoError(err)
	err = gzData.Flush()
	s.Require().NoError(err)
	err = gzData.Close()
	s.Require().NoError(err)
	objSrc := &readCloserMock{Buffer: buf}

	st := new(testutils.StorageMock)
	st.On("GetObject", mock.Anything, mock.Anything).
		Return(objSrc, nil)

	rb := newRestoreBase(&toc.Entry{
		Namespace: &schemaName,
		Tag:       &tableName,
		FileName:  &fileName,
	}, st, &pgrestore.DataSectionSettings{})
	ctx := context.Background()
	obj, err := rb.getObject(ctx)
	s.Require().NoError(err)
	readBuf := make([]byte, 1024)
	n, err := obj.Read(readBuf)
	s.Require().NoError(err)
	s.Assert().Equal(data, string(readBuf[:n]))
	s.NoError(obj.Close())
}

func TestRestorers(t *testing.T) {
	suite.Run(t, new(restoresSuite))
}
