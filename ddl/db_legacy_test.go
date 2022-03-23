// Copyright 2015 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ddl_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	. "github.com/pingcap/check"
	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/tidb/ddl"
	"github.com/pingcap/tidb/domain"
	"github.com/pingcap/tidb/errno"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/meta/autoid"
	"github.com/pingcap/tidb/parser/auth"
	"github.com/pingcap/tidb/parser/model"
	"github.com/pingcap/tidb/parser/mysql"
	parsertypes "github.com/pingcap/tidb/parser/types"
	"github.com/pingcap/tidb/planner/core"
	"github.com/pingcap/tidb/session"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/store/mockstore"
	"github.com/pingcap/tidb/table"
	"github.com/pingcap/tidb/util/dbterror"
	"github.com/pingcap/tidb/util/sqlexec"
	"github.com/pingcap/tidb/util/testkit"
	"github.com/pingcap/tidb/util/testleak"
	"github.com/stretchr/testify/require"
	"github.com/tikv/client-go/v2/testutils"
)

func TestT(t *testing.T) {
	CustomVerboseFlag = true
	*CustomParallelSuiteFlag = true

	testleak.BeforeTest()
	TestingT(t)
	testleak.AfterTestT(t)()
}

const (
	// waitForCleanDataRound indicates how many times should we check data is cleaned or not.
	waitForCleanDataRound = 150
	// waitForCleanDataInterval is a min duration between 2 check for data clean.
	waitForCleanDataInterval = time.Millisecond * 100
)

var _ = SerialSuites(&testSerialDBSuite{&testDBSuite{}})

const defaultBatchSize = 1024
const defaultReorgBatchSize = 256

type testDBSuite struct {
	cluster    testutils.Cluster
	store      kv.Storage
	dom        *domain.Domain
	schemaName string
	s          session.Session
	lease      time.Duration
	autoIDStep int64
	ctx        sessionctx.Context
}

func setUpSuite(s *testDBSuite, c *C) {
	var err error

	s.lease = 600 * time.Millisecond
	session.SetSchemaLease(s.lease)
	session.DisableStats4Test()
	s.schemaName = "test_db"
	s.autoIDStep = autoid.GetStep()
	ddl.SetWaitTimeWhenErrorOccurred(0)

	s.store, err = mockstore.NewMockStore(
		mockstore.WithClusterInspector(func(c testutils.Cluster) {
			mockstore.BootstrapWithSingleStore(c)
			s.cluster = c
		}),
	)
	c.Assert(err, IsNil)

	s.dom, err = session.BootstrapSession(s.store)
	c.Assert(err, IsNil)
	s.s, err = session.CreateSession4Test(s.store)
	c.Assert(err, IsNil)
	s.ctx = s.s.(sessionctx.Context)

	_, err = s.s.Execute(context.Background(), "create database test_db")
	c.Assert(err, IsNil)
	_, err = s.s.Execute(context.Background(), "set @@global.tidb_max_delta_schema_count= 4096")
	c.Assert(err, IsNil)
}

func tearDownSuite(s *testDBSuite, c *C) {
	_, err := s.s.Execute(context.Background(), "drop database if exists test_db")
	c.Assert(err, IsNil)
	s.s.Close()
	s.dom.Close()
	err = s.store.Close()
	c.Assert(err, IsNil)
}

func (s *testDBSuite) SetUpSuite(c *C) {
	setUpSuite(s, c)
}

func (s *testDBSuite) TearDownSuite(c *C) {
	tearDownSuite(s, c)
}

type testSerialDBSuite struct{ *testDBSuite }

func testGetTableByName(c *C, ctx sessionctx.Context, db, table string) table.Table {
	dom := domain.GetDomain(ctx)
	// Make sure the table schema is the new schema.
	err := dom.Reload()
	c.Assert(err, IsNil)
	tbl, err := dom.InfoSchema().TableByName(model.NewCIStr(db), model.NewCIStr(table))
	c.Assert(err, IsNil)
	return tbl
}

func backgroundExec(s kv.Storage, sql string, done chan error) {
	se, err := session.CreateSession4Test(s)
	if err != nil {
		done <- errors.Trace(err)
		return
	}
	defer se.Close()
	_, err = se.Execute(context.Background(), "use test")
	if err != nil {
		done <- errors.Trace(err)
		return
	}
	_, err = se.Execute(context.Background(), sql)
	done <- errors.Trace(err)
}

func testGetIndexID(t *testing.T, ctx sessionctx.Context, dbName, tblName, idxName string) int64 {
	is := domain.GetDomain(ctx).InfoSchema()
	tt, err := is.TableByName(model.NewCIStr(dbName), model.NewCIStr(tblName))
	require.NoError(t, err)

	for _, idx := range tt.Indices() {
		if idx.Meta().Name.L == idxName {
			return idx.Meta().ID
		}
	}
	require.FailNowf(t, "index %s not found(db: %s, tbl: %s)", idxName, dbName, tblName)
	return -1
}

func (s *testSerialDBSuite) TestRebaseAutoID(c *C) {
	c.Assert(failpoint.Enable("github.com/pingcap/tidb/meta/autoid/mockAutoIDChange", `return(true)`), IsNil)
	defer func() {
		c.Assert(failpoint.Disable("github.com/pingcap/tidb/meta/autoid/mockAutoIDChange"), IsNil)
	}()
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("use " + s.schemaName)

	tk.MustExec("drop database if exists tidb;")
	tk.MustExec("create database tidb;")
	tk.MustExec("use tidb;")
	tk.MustExec("create table tidb.test (a int auto_increment primary key, b int);")
	tk.MustExec("insert tidb.test values (null, 1);")
	tk.MustQuery("select * from tidb.test").Check(testkit.Rows("1 1"))
	tk.MustExec("alter table tidb.test auto_increment = 6000;")
	tk.MustExec("insert tidb.test values (null, 1);")
	tk.MustQuery("select * from tidb.test").Check(testkit.Rows("1 1", "6000 1"))
	tk.MustExec("alter table tidb.test auto_increment = 5;")
	tk.MustExec("insert tidb.test values (null, 1);")
	tk.MustQuery("select * from tidb.test").Check(testkit.Rows("1 1", "6000 1", "11000 1"))

	// Current range for table test is [11000, 15999].
	// Though it does not have a tuple "a = 15999", its global next auto increment id should be 16000.
	// Anyway it is not compatible with MySQL.
	tk.MustExec("alter table tidb.test auto_increment = 12000;")
	tk.MustExec("insert tidb.test values (null, 1);")
	tk.MustQuery("select * from tidb.test").Check(testkit.Rows("1 1", "6000 1", "11000 1", "16000 1"))

	tk.MustExec("create table tidb.test2 (a int);")
	tk.MustGetErrCode("alter table tidb.test2 add column b int auto_increment key, auto_increment=10;", errno.ErrUnsupportedDDLOperation)
}

func (s *testSerialDBSuite) TestProcessColumnFlags(c *C) {
	// check `processColumnFlags()`
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("use test_db")
	tk.MustExec("create table t(a year(4) comment 'xxx', b year, c bit)")
	defer tk.MustExec("drop table t;")

	check := func(n string, f func(uint) bool) {
		t := testGetTableByName(c, tk.Se, "test_db", "t")
		for _, col := range t.Cols() {
			if strings.EqualFold(col.Name.L, n) {
				c.Assert(f(col.Flag), IsTrue)
				break
			}
		}
	}

	yearcheck := func(f uint) bool {
		return mysql.HasUnsignedFlag(f) && mysql.HasZerofillFlag(f) && !mysql.HasBinaryFlag(f)
	}

	tk.MustExec("alter table t modify a year(4)")
	check("a", yearcheck)

	tk.MustExec("alter table t modify a year(4) unsigned")
	check("a", yearcheck)

	tk.MustExec("alter table t modify a year(4) zerofill")

	tk.MustExec("alter table t modify b year")
	check("b", yearcheck)

	tk.MustExec("alter table t modify c bit")
	check("c", func(f uint) bool {
		return mysql.HasUnsignedFlag(f) && !mysql.HasBinaryFlag(f)
	})
}

func (s *testSerialDBSuite) TestForbitCacheTableForSystemTable(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	sysTables := make([]string, 0, 24)
	memOrSysDB := []string{"MySQL", "INFORMATION_SCHEMA", "PERFORMANCE_SCHEMA", "METRICS_SCHEMA"}
	for _, db := range memOrSysDB {
		tk.MustExec("use " + db)
		tk.Se.Auth(&auth.UserIdentity{Username: "root", Hostname: "%"}, nil, nil)
		rows := tk.MustQuery("show tables").Rows()
		for i := 0; i < len(rows); i++ {
			sysTables = append(sysTables, rows[i][0].(string))
		}
		for _, one := range sysTables {
			_, err := tk.Exec(fmt.Sprintf("alter table `%s` cache", one))
			if db == "MySQL" {
				c.Assert(err.Error(), Equals, "[ddl:8200]ALTER table cache for tables in system database is currently unsupported")
			} else {
				c.Assert(err.Error(), Equals, fmt.Sprintf("[planner:1142]ALTER command denied to user 'root'@'%%' for table '%s'", strings.ToLower(one)))
			}

		}
		sysTables = sysTables[:0]
	}
}

func (s *testSerialDBSuite) TestAlterShardRowIDBits(c *C) {
	c.Assert(failpoint.Enable("github.com/pingcap/tidb/meta/autoid/mockAutoIDChange", `return(true)`), IsNil)
	defer func() {
		c.Assert(failpoint.Disable("github.com/pingcap/tidb/meta/autoid/mockAutoIDChange"), IsNil)
	}()

	tk := testkit.NewTestKit(c, s.store)

	tk.MustExec("use test")
	// Test alter shard_row_id_bits
	tk.MustExec("drop table if exists t1")
	defer tk.MustExec("drop table if exists t1")
	tk.MustExec("create table t1 (a int) shard_row_id_bits = 5")
	tk.MustExec(fmt.Sprintf("alter table t1 auto_increment = %d;", 1<<56))
	tk.MustExec("insert into t1 set a=1;")

	// Test increase shard_row_id_bits failed by overflow global auto ID.
	_, err := tk.Exec("alter table t1 SHARD_ROW_ID_BITS = 10;")
	c.Assert(err, NotNil)
	c.Assert(err.Error(), Equals, "[autoid:1467]shard_row_id_bits 10 will cause next global auto ID 72057594037932936 overflow")

	// Test reduce shard_row_id_bits will be ok.
	tk.MustExec("alter table t1 SHARD_ROW_ID_BITS = 3;")
	checkShardRowID := func(maxShardRowIDBits, shardRowIDBits uint64) {
		tbl := testGetTableByName(c, tk.Se, "test", "t1")
		c.Assert(tbl.Meta().MaxShardRowIDBits == maxShardRowIDBits, IsTrue)
		c.Assert(tbl.Meta().ShardRowIDBits == shardRowIDBits, IsTrue)
	}
	checkShardRowID(5, 3)

	// Test reduce shard_row_id_bits but calculate overflow should use the max record shard_row_id_bits.
	tk.MustExec("drop table if exists t1")
	tk.MustExec("create table t1 (a int) shard_row_id_bits = 10")
	tk.MustExec("alter table t1 SHARD_ROW_ID_BITS = 5;")
	checkShardRowID(10, 5)
	tk.MustExec(fmt.Sprintf("alter table t1 auto_increment = %d;", 1<<56))
	_, err = tk.Exec("insert into t1 set a=1;")
	c.Assert(err, NotNil)
	c.Assert(err.Error(), Equals, "[autoid:1467]Failed to read auto-increment value from storage engine")
}

func (s *testSerialDBSuite) TestShardRowIDBitsOnTemporaryTable(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("use test")
	// for global temporary table
	tk.MustExec("drop table if exists shard_row_id_temporary")
	_, err := tk.Exec("create global temporary table shard_row_id_temporary (a int) shard_row_id_bits = 5 on commit delete rows;")
	c.Assert(err.Error(), Equals, core.ErrOptOnTemporaryTable.GenWithStackByArgs("shard_row_id_bits").Error())
	tk.MustExec("create global temporary table shard_row_id_temporary (a int) on commit delete rows;")
	defer tk.MustExec("drop table if exists shard_row_id_temporary")
	_, err = tk.Exec("alter table shard_row_id_temporary shard_row_id_bits = 4;")
	c.Assert(err.Error(), Equals, dbterror.ErrOptOnTemporaryTable.GenWithStackByArgs("shard_row_id_bits").Error())
	// for local temporary table
	tk.MustExec("drop table if exists local_shard_row_id_temporary")
	_, err = tk.Exec("create temporary table local_shard_row_id_temporary (a int) shard_row_id_bits = 5;")
	c.Assert(err.Error(), Equals, core.ErrOptOnTemporaryTable.GenWithStackByArgs("shard_row_id_bits").Error())
	tk.MustExec("create temporary table local_shard_row_id_temporary (a int);")
	defer tk.MustExec("drop table if exists local_shard_row_id_temporary")
	_, err = tk.Exec("alter table local_shard_row_id_temporary shard_row_id_bits = 4;")
	c.Assert(err.Error(), Equals, dbterror.ErrUnsupportedLocalTempTableDDL.GenWithStackByArgs("ALTER TABLE").Error())
}

func (s *testSerialDBSuite) TestDDLJobErrorCount(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists ddl_error_table, new_ddl_error_table")
	tk.MustExec("create table ddl_error_table(a int)")

	c.Assert(failpoint.Enable("github.com/pingcap/tidb/ddl/mockErrEntrySizeTooLarge", `return(true)`), IsNil)
	defer func() {
		c.Assert(failpoint.Disable("github.com/pingcap/tidb/ddl/mockErrEntrySizeTooLarge"), IsNil)
	}()

	var jobID int64
	hook := &ddl.TestDDLCallback{}
	hook.OnJobUpdatedExported = func(job *model.Job) {
		jobID = job.ID
	}
	originHook := s.dom.DDL().GetHook()
	s.dom.DDL().SetHook(hook)
	defer s.dom.DDL().SetHook(originHook)

	tk.MustGetErrCode("rename table ddl_error_table to new_ddl_error_table", errno.ErrEntryTooLarge)

	historyJob, err := getHistoryDDLJob(s.store, jobID)
	c.Assert(err, IsNil)
	c.Assert(historyJob, NotNil)
	c.Assert(historyJob.ErrorCount, Equals, int64(1), Commentf("%v", historyJob))
	kv.ErrEntryTooLarge.Equal(historyJob.Error)
}

func (s *testSerialDBSuite) TestCommitTxnWithIndexChange(c *C) {
	// Prepare work.
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("set tidb_enable_amend_pessimistic_txn = 1;")
	tk.MustExec("drop database if exists test_db")
	tk.MustExec("create database test_db")
	tk.MustExec("use test_db")
	tk.MustExec("create table t1 (c1 int primary key, c2 int, c3 int, index ok2(c2))")
	tk.MustExec("insert t1 values (1, 10, 100), (2, 20, 200)")
	tk.MustExec("alter table t1 add index k2(c2)")
	tk.MustExec("alter table t1 drop index k2")
	tk.MustExec("alter table t1 add index k2(c2)")
	tk.MustExec("alter table t1 drop index k2")
	tk2 := testkit.NewTestKit(c, s.store)
	tk2.MustExec("use test_db")

	// tkSQLs are the sql statements for the pessimistic transaction.
	// tk2DDL are the ddl statements executed before the pessimistic transaction.
	// idxDDL is the DDL statement executed between pessimistic transaction begin and commit.
	// failCommit means the pessimistic transaction commit should fail not.
	type caseUnit struct {
		tkSQLs     []string
		tk2DDL     []string
		idxDDL     string
		checkSQLs  []string
		rowsExps   [][]string
		failCommit bool
		stateEnd   model.SchemaState
	}

	cases := []caseUnit{
		// Test secondary index
		{[]string{"insert into t1 values(3, 30, 300)",
			"insert into t2 values(11, 11, 11)"},
			[]string{"alter table t1 add index k2(c2)",
				"alter table t1 drop index k2",
				"alter table t1 add index kk2(c2, c1)",
				"alter table t1 add index k2(c2)",
				"alter table t1 drop index k2"},
			"alter table t1 add index k2(c2)",
			[]string{"select c3, c2 from t1 use index(k2) where c2 = 20",
				"select c3, c2 from t1 use index(k2) where c2 = 10",
				"select * from t1",
				"select * from t2 where c1 = 11"},
			[][]string{{"200 20"},
				{"100 10"},
				{"1 10 100", "2 20 200", "3 30 300"},
				{"11 11 11"}},
			false,
			model.StateNone},
		// Test secondary index
		{[]string{"insert into t2 values(5, 50, 500)",
			"insert into t2 values(11, 11, 11)",
			"delete from t2 where c2 = 11",
			"update t2 set c2 = 110 where c1 = 11"},
			// "update t2 set c1 = 10 where c3 = 100"},
			[]string{"alter table t1 add index k2(c2)",
				"alter table t1 drop index k2",
				"alter table t1 add index kk2(c2, c1)",
				"alter table t1 add index k2(c2)",
				"alter table t1 drop index k2"},
			"alter table t1 add index k2(c2)",
			[]string{"select c3, c2 from t1 use index(k2) where c2 = 20",
				"select c3, c2 from t1 use index(k2) where c2 = 10",
				"select * from t1",
				"select * from t2 where c1 = 11",
				"select * from t2 where c3 = 100"},
			[][]string{{"200 20"},
				{"100 10"},
				{"1 10 100", "2 20 200"},
				{},
				{"1 10 100"}},
			false,
			model.StateNone},
		// Test unique index
		{[]string{"insert into t1 values(3, 30, 300)",
			"insert into t1 values(4, 40, 400)",
			"insert into t2 values(11, 11, 11)",
			"insert into t2 values(12, 12, 11)"},
			[]string{"alter table t1 add unique index uk3(c3)",
				"alter table t1 drop index uk3",
				"alter table t2 add unique index ukc1c3(c1, c3)",
				"alter table t2 add unique index ukc3(c3)",
				"alter table t2 drop index ukc1c3",
				"alter table t2 drop index ukc3",
				"alter table t2 add index kc3(c3)"},
			"alter table t1 add unique index uk3(c3)",
			[]string{"select c3, c2 from t1 use index(uk3) where c3 = 200",
				"select c3, c2 from t1 use index(uk3) where c3 = 300",
				"select c3, c2 from t1 use index(uk3) where c3 = 400",
				"select * from t1",
				"select * from t2"},
			[][]string{{"200 20"},
				{"300 30"},
				{"400 40"},
				{"1 10 100", "2 20 200", "3 30 300", "4 40 400"},
				{"1 10 100", "2 20 200", "11 11 11", "12 12 11"}},
			false, model.StateNone},
		// Test unique index fail to commit, this case needs the new index could be inserted
		{[]string{"insert into t1 values(3, 30, 300)",
			"insert into t1 values(4, 40, 300)",
			"insert into t2 values(11, 11, 11)",
			"insert into t2 values(12, 11, 12)"},
			//[]string{"alter table t1 add unique index uk3(c3)", "alter table t1 drop index uk3"},
			[]string{},
			"alter table t1 add unique index uk3(c3)",
			[]string{"select c3, c2 from t1 use index(uk3) where c3 = 200",
				"select c3, c2 from t1 use index(uk3) where c3 = 300",
				"select c3, c2 from t1 where c1 = 4",
				"select * from t1",
				"select * from t2"},
			[][]string{{"200 20"},
				{},
				{},
				{"1 10 100", "2 20 200"},
				{"1 10 100", "2 20 200"}},
			true,
			model.StateWriteOnly},
	}
	tk.MustQuery("select * from t1;").Check(testkit.Rows("1 10 100", "2 20 200"))

	// Test add index state change
	do := s.dom.DDL()
	startStates := []model.SchemaState{model.StateNone, model.StateDeleteOnly}
	for _, startState := range startStates {
		endStatMap := session.ConstOpAddIndex[startState]
		var endStates []model.SchemaState
		for st := range endStatMap {
			endStates = append(endStates, st)
		}
		sort.Slice(endStates, func(i, j int) bool { return endStates[i] < endStates[j] })
		for _, endState := range endStates {
			for _, curCase := range cases {
				if endState < curCase.stateEnd {
					break
				}
				tk2.MustExec("drop table if exists t1")
				tk2.MustExec("drop table if exists t2")
				tk2.MustExec("create table t1 (c1 int primary key, c2 int, c3 int, index ok2(c2))")
				tk2.MustExec("create table t2 (c1 int primary key, c2 int, c3 int, index ok2(c2))")
				tk2.MustExec("insert t1 values (1, 10, 100), (2, 20, 200)")
				tk2.MustExec("insert t2 values (1, 10, 100), (2, 20, 200)")
				tk2.MustQuery("select * from t1;").Check(testkit.Rows("1 10 100", "2 20 200"))
				tk.MustQuery("select * from t1;").Check(testkit.Rows("1 10 100", "2 20 200"))
				tk.MustQuery("select * from t2;").Check(testkit.Rows("1 10 100", "2 20 200"))

				for _, DDLSQL := range curCase.tk2DDL {
					tk2.MustExec(DDLSQL)
				}
				hook := &ddl.TestDDLCallback{}
				prepared := false
				committed := false
				hook.OnJobUpdatedExported = func(job *model.Job) {
					if job.SchemaState == startState {
						if !prepared {
							tk.MustExec("begin pessimistic")
							for _, tkSQL := range curCase.tkSQLs {
								tk.MustExec(tkSQL)
							}
							prepared = true
						}
					} else if job.SchemaState == endState {
						if !committed {
							if curCase.failCommit {
								_, err := tk.Exec("commit")
								c.Assert(err, NotNil)
							} else {
								tk.MustExec("commit")
							}
						}
						committed = true
					}
				}
				originalCallback := do.GetHook()
				do.SetHook(hook)
				tk2.MustExec(curCase.idxDDL)
				do.SetHook(originalCallback)
				tk2.MustExec("admin check table t1")
				for i, checkSQL := range curCase.checkSQLs {
					if len(curCase.rowsExps[i]) > 0 {
						tk2.MustQuery(checkSQL).Check(testkit.Rows(curCase.rowsExps[i]...))
					} else {
						tk2.MustQuery(checkSQL).Check(nil)
					}
				}
			}
		}
	}
	tk.MustExec("admin check table t1")
}

// TestAddIndexFailOnCaseWhenCanExit is used to close #19325.
func (s *testSerialDBSuite) TestAddIndexFailOnCaseWhenCanExit(c *C) {
	c.Assert(failpoint.Enable("github.com/pingcap/tidb/ddl/MockCaseWhenParseFailure", `return(true)`), IsNil)
	defer func() {
		c.Assert(failpoint.Disable("github.com/pingcap/tidb/ddl/MockCaseWhenParseFailure"), IsNil)
	}()
	tk := testkit.NewTestKit(c, s.store)
	originalVal := variable.GetDDLErrorCountLimit()
	tk.MustExec("set @@global.tidb_ddl_error_count_limit = 1")
	defer tk.MustExec(fmt.Sprintf("set @@global.tidb_ddl_error_count_limit = %d", originalVal))

	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(a int, b int)")
	tk.MustExec("insert into t values(1, 1)")
	_, err := tk.Exec("alter table t add index idx(b)")
	c.Assert(err, NotNil)
	c.Assert(err.Error(), Equals, "[ddl:-1]DDL job rollback, error msg: job.ErrCount:1, mock unknown type: ast.whenClause.")
	tk.MustExec("drop table if exists t")
}

func (s *testSerialDBSuite) TestCreateTableWithIntegerLengthWaring(c *C) {
	// Inject the strict-integer-display-width variable in parser directly.
	parsertypes.TiDBStrictIntegerDisplayWidth = true
	defer func() {
		parsertypes.TiDBStrictIntegerDisplayWidth = false
	}()
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")

	tk.MustExec("create table t(a tinyint(1))")
	tk.MustQuery("show warnings").Check(testkit.Rows("Warning 1064 You have an error in your SQL syntax; check the manual that corresponds to your TiDB version for the right syntax to use [parser:1681]Integer display width is deprecated and will be removed in a future release."))

	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(a smallint(2))")
	tk.MustQuery("show warnings").Check(testkit.Rows("Warning 1064 You have an error in your SQL syntax; check the manual that corresponds to your TiDB version for the right syntax to use [parser:1681]Integer display width is deprecated and will be removed in a future release."))

	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(a int(2))")
	tk.MustQuery("show warnings").Check(testkit.Rows("Warning 1064 You have an error in your SQL syntax; check the manual that corresponds to your TiDB version for the right syntax to use [parser:1681]Integer display width is deprecated and will be removed in a future release."))

	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(a mediumint(2))")
	tk.MustQuery("show warnings").Check(testkit.Rows("Warning 1064 You have an error in your SQL syntax; check the manual that corresponds to your TiDB version for the right syntax to use [parser:1681]Integer display width is deprecated and will be removed in a future release."))

	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(a bigint(2))")
	tk.MustQuery("show warnings").Check(testkit.Rows("Warning 1064 You have an error in your SQL syntax; check the manual that corresponds to your TiDB version for the right syntax to use [parser:1681]Integer display width is deprecated and will be removed in a future release."))

	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(a integer(2))")
	tk.MustQuery("show warnings").Check(testkit.Rows("Warning 1064 You have an error in your SQL syntax; check the manual that corresponds to your TiDB version for the right syntax to use [parser:1681]Integer display width is deprecated and will be removed in a future release."))

	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(a int1(1))")
	tk.MustQuery("show warnings").Check(testkit.Rows("Warning 1064 You have an error in your SQL syntax; check the manual that corresponds to your TiDB version for the right syntax to use [parser:1681]Integer display width is deprecated and will be removed in a future release."))

	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(a int2(2))")
	tk.MustQuery("show warnings").Check(testkit.Rows("Warning 1064 You have an error in your SQL syntax; check the manual that corresponds to your TiDB version for the right syntax to use [parser:1681]Integer display width is deprecated and will be removed in a future release."))

	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(a int3(2))")
	tk.MustQuery("show warnings").Check(testkit.Rows("Warning 1064 You have an error in your SQL syntax; check the manual that corresponds to your TiDB version for the right syntax to use [parser:1681]Integer display width is deprecated and will be removed in a future release."))

	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(a int4(2))")
	tk.MustQuery("show warnings").Check(testkit.Rows("Warning 1064 You have an error in your SQL syntax; check the manual that corresponds to your TiDB version for the right syntax to use [parser:1681]Integer display width is deprecated and will be removed in a future release."))

	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(a int8(2))")
	tk.MustQuery("show warnings").Check(testkit.Rows("Warning 1064 You have an error in your SQL syntax; check the manual that corresponds to your TiDB version for the right syntax to use [parser:1681]Integer display width is deprecated and will be removed in a future release."))

	tk.MustExec("drop table if exists t")
}

// Close issue #24172.
// See https://github.com/pingcap/tidb/issues/24172
func (s *testSerialDBSuite) TestCancelJobWriteConflict(c *C) {
	tk := testkit.NewTestKitWithInit(c, s.store)
	tk1 := testkit.NewTestKitWithInit(c, s.store)
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(id int)")

	var cancelErr error
	var rs []sqlexec.RecordSet
	hook := &ddl.TestDDLCallback{}
	d := s.dom.DDL()
	originalHook := d.GetHook()
	d.SetHook(hook)
	defer d.SetHook(originalHook)

	// Test when cancelling cannot be retried and adding index succeeds.
	hook.OnJobRunBeforeExported = func(job *model.Job) {
		if job.Type == model.ActionAddIndex && job.State == model.JobStateRunning && job.SchemaState == model.StateWriteReorganization {
			stmt := fmt.Sprintf("admin cancel ddl jobs %d", job.ID)
			c.Assert(failpoint.Enable("github.com/pingcap/tidb/kv/mockCommitErrorInNewTxn", `return("no_retry")`), IsNil)
			defer func() { c.Assert(failpoint.Disable("github.com/pingcap/tidb/kv/mockCommitErrorInNewTxn"), IsNil) }()
			rs, cancelErr = tk1.Se.Execute(context.Background(), stmt)
		}
	}
	tk.MustExec("alter table t add index (id)")
	c.Assert(cancelErr.Error(), Equals, "mock commit error")

	// Test when cancelling is retried only once and adding index is cancelled in the end.
	var jobID int64
	hook.OnJobRunBeforeExported = func(job *model.Job) {
		if job.Type == model.ActionAddIndex && job.State == model.JobStateRunning && job.SchemaState == model.StateWriteReorganization {
			jobID = job.ID
			stmt := fmt.Sprintf("admin cancel ddl jobs %d", job.ID)
			c.Assert(failpoint.Enable("github.com/pingcap/tidb/kv/mockCommitErrorInNewTxn", `return("retry_once")`), IsNil)
			defer func() { c.Assert(failpoint.Disable("github.com/pingcap/tidb/kv/mockCommitErrorInNewTxn"), IsNil) }()
			rs, cancelErr = tk1.Se.Execute(context.Background(), stmt)
		}
	}
	tk.MustGetErrCode("alter table t add index (id)", errno.ErrCancelledDDLJob)
	c.Assert(cancelErr, IsNil)
	result := tk1.ResultSetToResultWithCtx(context.Background(), rs[0], Commentf("cancel ddl job fails"))
	result.Check(testkit.Rows(fmt.Sprintf("%d successful", jobID)))
}
