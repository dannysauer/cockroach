// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package sql_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/bootstrap"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/desctestutils"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/testcluster"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/stretchr/testify/require"
)

func TestTableRollback(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	s, sqlDB, kv := serverutils.StartServer(
		t, base.TestServerArgs{UseDatabase: "test"})
	defer s.Stopper().Stop(context.Background())
	execCfg := s.ExecutorConfig().(sql.ExecutorConfig)

	db := sqlutils.MakeSQLRunner(sqlDB)
	db.Exec(t, `CREATE DATABASE IF NOT EXISTS test`)
	db.Exec(t, `CREATE TABLE test (k INT PRIMARY KEY, rev INT DEFAULT 0, INDEX (rev))`)

	// Fill a table with some rows plus some revisions to those rows.
	const numRows = 1000
	db.Exec(t, `INSERT INTO test (k) SELECT generate_series(1, $1)`, numRows)
	db.Exec(t, `UPDATE test SET rev = 1 WHERE k % 3 = 0`)
	db.Exec(t, `DELETE FROM test WHERE k % 10 = 0`)
	db.Exec(t, `ALTER TABLE test SPLIT AT VALUES (30), (300), (501), (700)`)

	var ts string
	var before int
	db.QueryRow(t, `SELECT cluster_logical_timestamp(), xor_agg(k # rev) FROM test`).Scan(&ts, &before)
	targetTime, err := hlc.ParseHLC(ts)
	require.NoError(t, err)

	beforeNumRows := db.QueryStr(t, `SELECT count(*) FROM test`)

	// Make some more edits: delete some rows and edit others, insert into some of
	// the gaps made between previous rows, edit a large swath of rows and add a
	// large swath of new rows as well.
	db.Exec(t, `DELETE FROM test WHERE k % 5 = 2`)
	db.Exec(t, `INSERT INTO test (k, rev) SELECT generate_series(10, $1, 10), 10`, numRows)
	db.Exec(t, `INSERT INTO test (k, rev) SELECT generate_series($1+1, $1+500, 1), 500`, numRows)

	t.Run("simple-revert", func(t *testing.T) {

		const ignoreGC = false
		db.Exec(t, `UPDATE test SET rev = 2 WHERE k % 4 = 0`)
		db.Exec(t, `UPDATE test SET rev = 4 WHERE k > 150 and k < 350`)

		var edited, aost int
		db.QueryRow(t, `SELECT xor_agg(k # rev) FROM test`).Scan(&edited)
		require.NotEqual(t, before, edited)
		db.QueryRow(t, fmt.Sprintf(`SELECT xor_agg(k # rev) FROM test AS OF SYSTEM TIME %s`, ts)).Scan(&aost)
		require.Equal(t, before, aost)

		// Revert the table to ts.
		desc := desctestutils.TestingGetPublicTableDescriptor(kv, keys.SystemSQLCodec, "test", "test")
		desc.TableDesc().State = descpb.DescriptorState_OFFLINE // bypass the offline check.
		require.NoError(t, sql.RevertTables(context.Background(), kv, &execCfg, []catalog.TableDescriptor{desc}, targetTime, ignoreGC, 10))

		var reverted int
		db.QueryRow(t, `SELECT xor_agg(k # rev) FROM test`).Scan(&reverted)
		require.Equal(t, before, reverted, "expected reverted table after edits to match before")

		db.CheckQueryResults(t, `SELECT count(*) FROM test`, beforeNumRows)
	})

	t.Run("simple-delete-range-predicate", func(t *testing.T) {

		// Delete all keys with values after the targetTime
		desc := desctestutils.TestingGetPublicTableDescriptor(kv, keys.SystemSQLCodec, "test", "test")

		predicates := kvpb.DeleteRangePredicates{StartTime: targetTime}
		require.NoError(t, sql.DeleteTableWithPredicate(context.Background(), kv, execCfg.Codec,
			&s.ClusterSettings().SV, execCfg.DistSender, desc, predicates, 10))

		db.CheckQueryResults(t, `SELECT count(*) FROM test`, beforeNumRows)
	})
}

func TestRevertGCThreshold(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	tc := testcluster.StartTestCluster(t, 1, base.TestClusterArgs{})
	defer tc.Stopper().Stop(ctx)
	kvDB := tc.Server(0).DB()

	req := &kvpb.RevertRangeRequest{
		RequestHeader: kvpb.RequestHeader{Key: bootstrap.TestingUserTableDataMin(), EndKey: keys.MaxKey},
		TargetTime:    hlc.Timestamp{WallTime: -1},
	}
	_, pErr := kv.SendWrapped(ctx, kvDB.NonTransactionalSender(), req)
	if !testutils.IsPError(pErr, "must be after replica GC threshold") {
		t.Fatalf(`expected "must be after replica GC threshold" error got: %+v`, pErr)
	}
	req.IgnoreGcThreshold = true
	_, pErr = kv.SendWrapped(ctx, kvDB.NonTransactionalSender(), req)
	require.Nil(t, pErr)
}
