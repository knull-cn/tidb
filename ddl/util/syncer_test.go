// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package util_test

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	. "github.com/pingcap/check"
	"github.com/pingcap/errors"
	"github.com/pingcap/parser/terror"
	. "github.com/pingcap/tidb/ddl"
	. "github.com/pingcap/tidb/ddl/util"
	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/owner"
	"github.com/pingcap/tidb/store/mockstore"
	"go.etcd.io/etcd/clientv3"
	"go.etcd.io/etcd/etcdserver"
	"go.etcd.io/etcd/integration"
	"go.etcd.io/etcd/mvcc/mvccpb"
	goctx "golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestT(t *testing.T) {
	TestingT(t)
}

const minInterval = 10 * time.Nanosecond // It's used to test timeout.

func TestSyncerSimple(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("integration.NewClusterV3 will create file contains a colon which is not allowed on Windows")
	}
	testLease := 5 * time.Millisecond
	origin := CheckVersFirstWaitTime
	CheckVersFirstWaitTime = 0
	defer func() {
		CheckVersFirstWaitTime = origin
	}()

	store, err := mockstore.NewMockStore()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		err := store.Close()
		if err != nil {
			t.Fatal(err)
		}
	}()

	clus := integration.NewClusterV3(t, &integration.ClusterConfig{Size: 1})
	defer clus.Terminate(t)
	cli := clus.RandClient()
	ctx := goctx.Background()
	ic := infoschema.NewCache(2)
	ic.Insert(infoschema.MockInfoSchemaWithSchemaVer(nil, 0))
	d := NewDDL(
		ctx,
		WithEtcdClient(cli),
		WithStore(store),
		WithLease(testLease),
		WithInfoCache(ic),
	)
	err = d.Start(nil)
	if err != nil {
		t.Fatalf("DDL start failed %v", err)
	}
	defer func() {
		err := d.Stop()
		if err != nil {
			t.Fatal(err)
		}
	}()

	// for init function
	if err = d.SchemaSyncer().Init(ctx); err != nil {
		t.Fatalf("schema version syncer init failed %v", err)
	}
	resp, err := cli.Get(ctx, DDLAllSchemaVersions, clientv3.WithPrefix())
	if err != nil {
		t.Fatalf("client get version failed %v", err)
	}
	key := DDLAllSchemaVersions + "/" + d.OwnerManager().ID()
	checkRespKV(t, 1, key, InitialVersion, resp.Kvs...)
	// for MustGetGlobalVersion function
	globalVer, err := d.SchemaSyncer().MustGetGlobalVersion(ctx)
	if err != nil {
		t.Fatalf("client get global version failed %v", err)
	}
	if InitialVersion != fmt.Sprintf("%d", globalVer) {
		t.Fatalf("client get global version %d isn't equal to init version %s", globalVer, InitialVersion)
	}
	childCtx, _ := goctx.WithTimeout(ctx, minInterval)
	_, err = d.SchemaSyncer().MustGetGlobalVersion(childCtx)
	if !isTimeoutError(err) {
		t.Fatalf("client get global version result not match, err %v", err)
	}

	ic2 := infoschema.NewCache(2)
	ic2.Insert(infoschema.MockInfoSchemaWithSchemaVer(nil, 0))
	d1 := NewDDL(
		ctx,
		WithEtcdClient(cli),
		WithStore(store),
		WithLease(testLease),
		WithInfoCache(ic2),
	)
	err = d1.Start(nil)
	if err != nil {
		t.Fatalf("DDL start failed %v", err)
	}
	defer func() {
		err := d.Stop()
		if err != nil {
			t.Fatal(err)
		}
	}()
	if err = d1.SchemaSyncer().Init(ctx); err != nil {
		t.Fatalf("schema version syncer init failed %v", err)
	}

	// for watchCh
	wg := sync.WaitGroup{}
	wg.Add(1)
	currentVer := int64(123)
	var checkErr string
	go func() {
		defer wg.Done()
		select {
		case resp := <-d.SchemaSyncer().GlobalVersionCh():
			if len(resp.Events) < 1 {
				checkErr = "get chan events count less than 1"
				return
			}
			checkRespKV(t, 1, DDLGlobalSchemaVersion, fmt.Sprintf("%v", currentVer), resp.Events[0].Kv)
		case <-time.After(3 * time.Second):
			checkErr = "get udpate version failed"
			return
		}
	}()

	// for update latestSchemaVersion
	err = d.SchemaSyncer().OwnerUpdateGlobalVersion(ctx, currentVer)
	if err != nil {
		t.Fatalf("update latest schema version failed %v", err)
	}

	wg.Wait()

	if checkErr != "" {
		t.Fatalf(checkErr)
	}

	// for CheckAllVersions
	childCtx, cancel := goctx.WithTimeout(ctx, 200*time.Millisecond)
	err = d.SchemaSyncer().OwnerCheckAllVersions(childCtx, currentVer)
	if err == nil {
		t.Fatalf("check result not match")
	}
	cancel()

	// for UpdateSelfVersion
	err = d.SchemaSyncer().UpdateSelfVersion(context.Background(), currentVer)
	if err != nil {
		t.Fatalf("update self version failed %v", errors.ErrorStack(err))
	}
	err = d1.SchemaSyncer().UpdateSelfVersion(context.Background(), currentVer)
	if err != nil {
		t.Fatalf("update self version failed %v", errors.ErrorStack(err))
	}
	childCtx, _ = goctx.WithTimeout(ctx, minInterval)
	err = d1.SchemaSyncer().UpdateSelfVersion(childCtx, currentVer)
	if !isTimeoutError(err) {
		t.Fatalf("update self version result not match, err %v", err)
	}

	// for CheckAllVersions
	err = d.SchemaSyncer().OwnerCheckAllVersions(context.Background(), currentVer-1)
	if err != nil {
		t.Fatalf("check all versions failed %v", err)
	}
	err = d.SchemaSyncer().OwnerCheckAllVersions(context.Background(), currentVer)
	if err != nil {
		t.Fatalf("check all versions failed %v", err)
	}
	childCtx, _ = goctx.WithTimeout(ctx, minInterval)
	err = d.SchemaSyncer().OwnerCheckAllVersions(childCtx, currentVer)
	if !isTimeoutError(err) {
		t.Fatalf("check all versions result not match, err %v", err)
	}

	// for StartCleanWork
	ttl := 10
	// Make sure NeededCleanTTL > ttl, then we definitely clean the ttl.
	NeededCleanTTL = int64(11)
	ttlKey := "session_ttl_key"
	ttlVal := "session_ttl_val"
	session, err := owner.NewSession(ctx, "", cli, owner.NewSessionDefaultRetryCnt, ttl)
	if err != nil {
		t.Fatalf("new session failed %v", err)
	}
	err = PutKVToEtcd(context.Background(), cli, 5, ttlKey, ttlVal, clientv3.WithLease(session.Lease()))
	if err != nil {
		t.Fatalf("put kv to etcd failed %v", err)
	}
	// Make sure the ttlKey is exist in etcd.
	resp, err = cli.Get(ctx, ttlKey)
	if err != nil {
		t.Fatalf("client get version failed %v", err)
	}
	checkRespKV(t, 1, ttlKey, ttlVal, resp.Kvs...)
	d.SchemaSyncer().NotifyCleanExpiredPaths()
	// Make sure the clean worker is done.
	notifiedCnt := 1
	for i := 0; i < 100; i++ {
		isNotified := d.SchemaSyncer().NotifyCleanExpiredPaths()
		if isNotified {
			notifiedCnt++
		}
		// notifyCleanExpiredPathsCh's length is 1,
		// so when notifiedCnt is 3, we can make sure the clean worker is done at least once.
		if notifiedCnt == 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if notifiedCnt != 3 {
		t.Fatal("clean worker don't finish")
	}
	// Make sure the ttlKey is removed in etcd.
	resp, err = cli.Get(ctx, ttlKey)
	if err != nil {
		t.Fatalf("client get version failed %v", err)
	}
	checkRespKV(t, 0, ttlKey, "", resp.Kvs...)

	// for Close
	resp, err = cli.Get(goctx.Background(), key)
	if err != nil {
		t.Fatalf("get key %s failed %v", key, err)
	}
	currVer := fmt.Sprintf("%v", currentVer)
	checkRespKV(t, 1, key, currVer, resp.Kvs...)
	d.SchemaSyncer().Close()
	resp, err = cli.Get(goctx.Background(), key)
	if err != nil {
		t.Fatalf("get key %s failed %v", key, err)
	}
	if len(resp.Kvs) != 0 {
		t.Fatalf("remove key %s failed %v", key, err)
	}
}

func isTimeoutError(err error) bool {
	if terror.ErrorEqual(err, goctx.DeadlineExceeded) || status.Code(errors.Cause(err)) == codes.DeadlineExceeded ||
		terror.ErrorEqual(err, etcdserver.ErrTimeout) {
		return true
	}
	return false
}

func checkRespKV(t *testing.T, kvCount int, key, val string,
	kvs ...*mvccpb.KeyValue) {
	if len(kvs) != kvCount {
		t.Fatalf("resp key %s kvs %v length is != %d", key, kvs, kvCount)
	}
	if kvCount == 0 {
		return
	}

	kv := kvs[0]
	if string(kv.Key) != key {
		t.Fatalf("key resp %s, exported %s", kv.Key, key)
	}
	if string(kv.Value) != val {
		t.Fatalf("val resp %s, exported %s", kv.Value, val)
	}
}
