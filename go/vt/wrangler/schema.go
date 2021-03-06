// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package wrangler

import (
	"bytes"
	"fmt"
	"html/template"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/context"

	log "github.com/golang/glog"
	"github.com/youtube/vitess/go/vt/concurrency"
	myproto "github.com/youtube/vitess/go/vt/mysqlctl/proto"
	"github.com/youtube/vitess/go/vt/tabletmanager/actionnode"
	"github.com/youtube/vitess/go/vt/topo"
	"github.com/youtube/vitess/go/vt/topotools"
	"github.com/youtube/vitess/go/vt/topotools/events"
)

// GetSchema uses an RPC to get the schema from a remote tablet
func (wr *Wrangler) GetSchema(ctx context.Context, tabletAlias topo.TabletAlias, tables, excludeTables []string, includeViews bool) (*myproto.SchemaDefinition, error) {
	ti, err := wr.ts.GetTablet(tabletAlias)
	if err != nil {
		return nil, err
	}

	return wr.tmc.GetSchema(ctx, ti, tables, excludeTables, includeViews)
}

// ReloadSchema forces the remote tablet to reload its schema.
func (wr *Wrangler) ReloadSchema(ctx context.Context, tabletAlias topo.TabletAlias) error {
	ti, err := wr.ts.GetTablet(tabletAlias)
	if err != nil {
		return err
	}

	return wr.tmc.ReloadSchema(ctx, ti)
}

// helper method to asynchronously diff a schema
func (wr *Wrangler) diffSchema(ctx context.Context, masterSchema *myproto.SchemaDefinition, masterTabletAlias, alias topo.TabletAlias, excludeTables []string, includeViews bool, wg *sync.WaitGroup, er concurrency.ErrorRecorder) {
	defer wg.Done()
	log.Infof("Gathering schema for %v", alias)
	slaveSchema, err := wr.GetSchema(ctx, alias, nil, excludeTables, includeViews)
	if err != nil {
		er.RecordError(err)
		return
	}

	log.Infof("Diffing schema for %v", alias)
	myproto.DiffSchema(masterTabletAlias.String(), masterSchema, alias.String(), slaveSchema, er)
}

// ValidateSchemaShard will diff the schema from all the tablets in the shard.
func (wr *Wrangler) ValidateSchemaShard(ctx context.Context, keyspace, shard string, excludeTables []string, includeViews bool) error {
	si, err := wr.ts.GetShard(keyspace, shard)
	if err != nil {
		return err
	}

	// get schema from the master, or error
	if si.MasterAlias.Uid == topo.NO_TABLET {
		return fmt.Errorf("No master in shard %v/%v", keyspace, shard)
	}
	log.Infof("Gathering schema for master %v", si.MasterAlias)
	masterSchema, err := wr.GetSchema(ctx, si.MasterAlias, nil, excludeTables, includeViews)
	if err != nil {
		return err
	}

	// read all the aliases in the shard, that is all tablets that are
	// replicating from the master
	aliases, err := topo.FindAllTabletAliasesInShard(ctx, wr.ts, keyspace, shard)
	if err != nil {
		return err
	}

	// then diff with all slaves
	er := concurrency.AllErrorRecorder{}
	wg := sync.WaitGroup{}
	for _, alias := range aliases {
		if alias == si.MasterAlias {
			continue
		}

		wg.Add(1)
		go wr.diffSchema(ctx, masterSchema, si.MasterAlias, alias, excludeTables, includeViews, &wg, &er)
	}
	wg.Wait()
	if er.HasErrors() {
		return fmt.Errorf("Schema diffs:\n%v", er.Error().Error())
	}
	return nil
}

// ValidateSchemaKeyspace will diff the schema from all the tablets in
// the keyspace.
func (wr *Wrangler) ValidateSchemaKeyspace(ctx context.Context, keyspace string, excludeTables []string, includeViews bool) error {
	// find all the shards
	shards, err := wr.ts.GetShardNames(keyspace)
	if err != nil {
		return err
	}

	// corner cases
	if len(shards) == 0 {
		return fmt.Errorf("No shards in keyspace %v", keyspace)
	}
	sort.Strings(shards)
	if len(shards) == 1 {
		return wr.ValidateSchemaShard(ctx, keyspace, shards[0], excludeTables, includeViews)
	}

	// find the reference schema using the first shard's master
	si, err := wr.ts.GetShard(keyspace, shards[0])
	if err != nil {
		return err
	}
	if si.MasterAlias.Uid == topo.NO_TABLET {
		return fmt.Errorf("No master in shard %v/%v", keyspace, shards[0])
	}
	referenceAlias := si.MasterAlias
	log.Infof("Gathering schema for reference master %v", referenceAlias)
	referenceSchema, err := wr.GetSchema(ctx, referenceAlias, nil, excludeTables, includeViews)
	if err != nil {
		return err
	}

	// then diff with all other tablets everywhere
	er := concurrency.AllErrorRecorder{}
	wg := sync.WaitGroup{}

	// first diff the slaves in the reference shard 0
	aliases, err := topo.FindAllTabletAliasesInShard(ctx, wr.ts, keyspace, shards[0])
	if err != nil {
		return err
	}

	for _, alias := range aliases {
		if alias == si.MasterAlias {
			continue
		}

		wg.Add(1)
		go wr.diffSchema(ctx, referenceSchema, referenceAlias, alias, excludeTables, includeViews, &wg, &er)
	}

	// then diffs all tablets in the other shards
	for _, shard := range shards[1:] {
		si, err := wr.ts.GetShard(keyspace, shard)
		if err != nil {
			er.RecordError(err)
			continue
		}

		if si.MasterAlias.Uid == topo.NO_TABLET {
			er.RecordError(fmt.Errorf("No master in shard %v/%v", keyspace, shard))
			continue
		}

		aliases, err := topo.FindAllTabletAliasesInShard(ctx, wr.ts, keyspace, shard)
		if err != nil {
			er.RecordError(err)
			continue
		}

		for _, alias := range aliases {
			wg.Add(1)
			go wr.diffSchema(ctx, referenceSchema, referenceAlias, alias, excludeTables, includeViews, &wg, &er)
		}
	}
	wg.Wait()
	if er.HasErrors() {
		return fmt.Errorf("Schema diffs:\n%v", er.Error().Error())
	}
	return nil
}

// PreflightSchema will try a schema change on the remote tablet.
func (wr *Wrangler) PreflightSchema(ctx context.Context, tabletAlias topo.TabletAlias, change string) (*myproto.SchemaChangeResult, error) {
	ti, err := wr.ts.GetTablet(tabletAlias)
	if err != nil {
		return nil, err
	}
	return wr.tmc.PreflightSchema(ctx, ti, change)
}

// ApplySchema will apply a schema change on the remote tablet.
func (wr *Wrangler) ApplySchema(ctx context.Context, tabletAlias topo.TabletAlias, sc *myproto.SchemaChange) (*myproto.SchemaChangeResult, error) {
	ti, err := wr.ts.GetTablet(tabletAlias)
	if err != nil {
		return nil, err
	}
	return wr.tmc.ApplySchema(ctx, ti, sc)
}

// ApplySchemaShard applies a shcema change on a shard.
// Note for 'complex' mode (the 'simple' mode is easy enough that we
// don't need to handle recovery that much): this method is able to
// recover if interrupted in the middle, because it knows which server
// has the schema change already applied, and will just pass through them
// very quickly.
func (wr *Wrangler) ApplySchemaShard(ctx context.Context, keyspace, shard, change string, newParentTabletAlias topo.TabletAlias, simple, force bool, waitSlaveTimeout time.Duration) (*myproto.SchemaChangeResult, error) {
	// read the shard
	shardInfo, err := wr.ts.GetShard(keyspace, shard)
	if err != nil {
		return nil, err
	}

	// preflight on the master, to get baseline
	// this assumes the master doesn't have the schema upgrade applied
	// If the master does, and some slaves don't, may have to
	// fix them manually one at a time, or re-clone them.
	// we do this outside of the shard lock because we can.
	log.Infof("Running Preflight on Master %v", shardInfo.MasterAlias)
	if err != nil {
		return nil, err
	}
	preflight, err := wr.PreflightSchema(ctx, shardInfo.MasterAlias, change)
	if err != nil {
		return nil, err
	}

	return wr.lockAndApplySchemaShard(ctx, shardInfo, preflight, keyspace, shard, shardInfo.MasterAlias, change, newParentTabletAlias, simple, force, waitSlaveTimeout)
}

func (wr *Wrangler) lockAndApplySchemaShard(ctx context.Context, shardInfo *topo.ShardInfo, preflight *myproto.SchemaChangeResult, keyspace, shard string, masterTabletAlias topo.TabletAlias, change string, newParentTabletAlias topo.TabletAlias, simple, force bool, waitSlaveTimeout time.Duration) (*myproto.SchemaChangeResult, error) {
	// get a shard lock
	actionNode := actionnode.ApplySchemaShard(masterTabletAlias, change, simple)
	lockPath, err := wr.lockShard(ctx, keyspace, shard, actionNode)
	if err != nil {
		return nil, err
	}

	scr, err := wr.applySchemaShard(ctx, shardInfo, preflight, masterTabletAlias, change, newParentTabletAlias, simple, force, waitSlaveTimeout)
	return scr, wr.unlockShard(ctx, keyspace, shard, actionNode, lockPath, err)
}

// tabletStatus is a local structure used to keep track of what we're doing
type tabletStatus struct {
	ti           *topo.TabletInfo
	lastError    error
	beforeSchema *myproto.SchemaDefinition
}

func (wr *Wrangler) applySchemaShard(ctx context.Context, shardInfo *topo.ShardInfo, preflight *myproto.SchemaChangeResult, masterTabletAlias topo.TabletAlias, change string, newParentTabletAlias topo.TabletAlias, simple, force bool, waitSlaveTimeout time.Duration) (*myproto.SchemaChangeResult, error) {

	// find all the shards we need to handle
	aliases, err := topo.FindAllTabletAliasesInShard(ctx, wr.ts, shardInfo.Keyspace(), shardInfo.ShardName())
	if err != nil {
		return nil, err
	}

	// build the array of tabletStatus we're going to use
	statusArray := make([]*tabletStatus, 0, len(aliases)-1)
	for _, alias := range aliases {
		if alias == masterTabletAlias {
			// we skip the master
			continue
		}

		ti, err := wr.ts.GetTablet(alias)
		if err != nil {
			return nil, err
		}
		if ti.Type == topo.TYPE_LAG {
			// lag tablets are usually behind, not replicating,
			// and a general pain. So let's just skip them
			// all together.
			// TODO(alainjobart) figure out other types to skip:
			// ValidateSchemaShard only does the serving types.
			// We do everything in the replication graph
			// but LAG. This seems fine for now.
			log.Infof("Skipping tablet %v as it is LAG", ti.Alias)
			continue
		}

		statusArray = append(statusArray, &tabletStatus{ti: ti})
	}

	// get schema on all tablets.
	log.Infof("Getting schema on all tablets for shard %v/%v", shardInfo.Keyspace(), shardInfo.ShardName())
	wg := &sync.WaitGroup{}
	for _, status := range statusArray {
		wg.Add(1)
		go func(status *tabletStatus) {
			status.beforeSchema, status.lastError = wr.tmc.GetSchema(ctx, status.ti, nil, nil, false)
			wg.Done()
		}(status)
	}
	wg.Wait()

	// quick check for errors
	for _, status := range statusArray {
		if status.lastError != nil {
			return nil, fmt.Errorf("Error getting schema on tablet %v: %v", status.ti.Alias, status.lastError)
		}
	}

	// simple or complex?
	if simple {
		return wr.applySchemaShardSimple(ctx, statusArray, preflight, masterTabletAlias, change, force)
	}

	return wr.applySchemaShardComplex(ctx, statusArray, shardInfo, preflight, masterTabletAlias, change, newParentTabletAlias, force, waitSlaveTimeout)
}

func (wr *Wrangler) applySchemaShardSimple(ctx context.Context, statusArray []*tabletStatus, preflight *myproto.SchemaChangeResult, masterTabletAlias topo.TabletAlias, change string, force bool) (*myproto.SchemaChangeResult, error) {
	// check all tablets have the same schema as the master's
	// BeforeSchema. If not, we shouldn't proceed
	log.Infof("Checking schema on all tablets")
	for _, status := range statusArray {
		diffs := myproto.DiffSchemaToArray("master", preflight.BeforeSchema, status.ti.Alias.String(), status.beforeSchema)
		if len(diffs) > 0 {
			if force {
				log.Warningf("Tablet %v has inconsistent schema, ignoring: %v", status.ti.Alias, strings.Join(diffs, "\n"))
			} else {
				return nil, fmt.Errorf("Tablet %v has inconsistent schema: %v", status.ti.Alias, strings.Join(diffs, "\n"))
			}
		}
	}

	// we're good, just send to the master
	log.Infof("Applying schema change to master in simple mode")
	sc := &myproto.SchemaChange{Sql: change, Force: force, AllowReplication: true, BeforeSchema: preflight.BeforeSchema, AfterSchema: preflight.AfterSchema}
	return wr.ApplySchema(ctx, masterTabletAlias, sc)
}

func (wr *Wrangler) applySchemaShardComplex(ctx context.Context, statusArray []*tabletStatus, shardInfo *topo.ShardInfo, preflight *myproto.SchemaChangeResult, masterTabletAlias topo.TabletAlias, change string, newParentTabletAlias topo.TabletAlias, force bool, waitSlaveTimeout time.Duration) (*myproto.SchemaChangeResult, error) {
	// apply the schema change to all replica / slave tablets
	for _, status := range statusArray {
		// if already applied, we skip this guy
		diffs := myproto.DiffSchemaToArray("after", preflight.AfterSchema, status.ti.Alias.String(), status.beforeSchema)
		if len(diffs) == 0 {
			log.Infof("Tablet %v already has the AfterSchema, skipping", status.ti.Alias)
			continue
		}

		// make sure the before schema matches
		diffs = myproto.DiffSchemaToArray("master", preflight.BeforeSchema, status.ti.Alias.String(), status.beforeSchema)
		if len(diffs) > 0 {
			if force {
				log.Warningf("Tablet %v has inconsistent schema, ignoring: %v", status.ti.Alias, strings.Join(diffs, "\n"))
			} else {
				return nil, fmt.Errorf("Tablet %v has inconsistent schema: %v", status.ti.Alias, strings.Join(diffs, "\n"))
			}
		}

		// take this guy out of the serving graph if necessary
		ti, err := wr.ts.GetTablet(status.ti.Alias)
		if err != nil {
			return nil, err
		}
		typeChangeRequired := ti.Tablet.IsInServingGraph()
		if typeChangeRequired {
			// note we want to update the serving graph there
			err = wr.changeTypeInternal(ctx, ti.Alias, topo.TYPE_SCHEMA_UPGRADE)
			if err != nil {
				return nil, err
			}
		}

		// apply the schema change
		log.Infof("Applying schema change to slave %v in complex mode", status.ti.Alias)
		sc := &myproto.SchemaChange{Sql: change, Force: force, AllowReplication: false, BeforeSchema: preflight.BeforeSchema, AfterSchema: preflight.AfterSchema}
		_, err = wr.ApplySchema(ctx, status.ti.Alias, sc)
		if err != nil {
			return nil, err
		}

		// put this guy back into the serving graph
		if typeChangeRequired {
			err = wr.changeTypeInternal(ctx, ti.Alias, ti.Tablet.Type)
			if err != nil {
				return nil, err
			}
		}
	}

	// if newParentTabletAlias is passed in, use that as the new master
	if !newParentTabletAlias.IsZero() {
		log.Infof("Reparenting with new master set to %v", newParentTabletAlias)
		tabletMap, err := topo.GetTabletMapForShard(ctx, wr.ts, shardInfo.Keyspace(), shardInfo.ShardName())
		if err != nil {
			return nil, err
		}

		slaveTabletMap, masterTabletMap := topotools.SortedTabletMap(tabletMap)
		newMasterTablet, err := wr.ts.GetTablet(newParentTabletAlias)
		if err != nil {
			return nil, err
		}

		// Create reusable Reparent event with available info
		ev := &events.Reparent{
			ShardInfo: *shardInfo,
			NewMaster: *newMasterTablet.Tablet,
		}

		if oldMasterTablet, ok := tabletMap[shardInfo.MasterAlias]; ok {
			ev.OldMaster = *oldMasterTablet.Tablet
		}

		err = wr.reparentShardGraceful(ctx, ev, shardInfo, slaveTabletMap, masterTabletMap, newMasterTablet /*leaveMasterReadOnly*/, false, waitSlaveTimeout)
		if err != nil {
			return nil, err
		}

		// Here we would apply the schema change to the old
		// master, but after a reparent it's in Scrap state,
		// so no need to.  When/if reparent leaves the
		// original master in a different state (like replica
		// or rdonly), then we should apply the schema there
		// too.
		log.Infof("Skipping schema change on old master %v in complex mode, it's been Scrapped", masterTabletAlias)
	}
	return &myproto.SchemaChangeResult{BeforeSchema: preflight.BeforeSchema, AfterSchema: preflight.AfterSchema}, nil
}

// ApplySchemaKeyspace applies a schema change to an entire keyspace.
// take a keyspace lock to do this.
// first we will validate the Preflight works the same on all shard masters
// and fail if not (unless force is specified)
// if simple, we just do it on all masters.
// if complex, we do the shell game in parallel on all shards
func (wr *Wrangler) ApplySchemaKeyspace(ctx context.Context, keyspace string, change string, simple, force bool, waitSlaveTimeout time.Duration) (*myproto.SchemaChangeResult, error) {
	actionNode := actionnode.ApplySchemaKeyspace(change, simple)
	lockPath, err := wr.lockKeyspace(ctx, keyspace, actionNode)
	if err != nil {
		return nil, err
	}

	scr, err := wr.applySchemaKeyspace(ctx, keyspace, change, simple, force, waitSlaveTimeout)
	return scr, wr.unlockKeyspace(ctx, keyspace, actionNode, lockPath, err)
}

func (wr *Wrangler) applySchemaKeyspace(ctx context.Context, keyspace string, change string, simple, force bool, waitSlaveTimeout time.Duration) (*myproto.SchemaChangeResult, error) {
	shards, err := wr.ts.GetShardNames(keyspace)
	if err != nil {
		return nil, err
	}

	// corner cases
	if len(shards) == 0 {
		return nil, fmt.Errorf("No shards in keyspace %v", keyspace)
	}
	if len(shards) == 1 {
		log.Infof("Only one shard in keyspace %v, using ApplySchemaShard", keyspace)
		return wr.ApplySchemaShard(ctx, keyspace, shards[0], change, topo.TabletAlias{}, simple, force, waitSlaveTimeout)
	}

	// Get schema on all shard masters in parallel
	log.Infof("Getting schema on all shards")
	beforeSchemas := make([]*myproto.SchemaDefinition, len(shards))
	shardInfos := make([]*topo.ShardInfo, len(shards))
	wg := sync.WaitGroup{}
	mu := sync.Mutex{}
	getErrs := make([]string, 0, 5)
	for i, shard := range shards {
		wg.Add(1)
		go func(i int, shard string) {
			var err error
			defer func() {
				if err != nil {
					mu.Lock()
					getErrs = append(getErrs, err.Error())
					mu.Unlock()
				}
				wg.Done()
			}()

			shardInfos[i], err = wr.ts.GetShard(keyspace, shard)
			if err != nil {
				return
			}

			beforeSchemas[i], err = wr.GetSchema(ctx, shardInfos[i].MasterAlias, nil, nil, false)
		}(i, shard)
	}
	wg.Wait()
	if len(getErrs) > 0 {
		return nil, fmt.Errorf("Error(s) getting schema: %v", strings.Join(getErrs, ", "))
	}

	// check they all match, or use the force flag
	log.Infof("Checking starting schemas match on all shards")
	for i, beforeSchema := range beforeSchemas {
		if i == 0 {
			continue
		}
		diffs := myproto.DiffSchemaToArray("shard 0", beforeSchemas[0], fmt.Sprintf("shard %v", i), beforeSchema)
		if len(diffs) > 0 {
			if force {
				log.Warningf("Shard %v has inconsistent schema, ignoring: %v", i, strings.Join(diffs, "\n"))
			} else {
				return nil, fmt.Errorf("Shard %v has inconsistent schema: %v", i, strings.Join(diffs, "\n"))
			}
		}
	}

	// preflight on shard 0 master, to get baseline
	// this assumes shard 0 master doesn't have the schema upgrade applied
	// if it does, we'll have to fix the slaves and other shards manually.
	log.Infof("Running Preflight on Shard 0 Master")
	preflight, err := wr.PreflightSchema(ctx, shardInfos[0].MasterAlias, change)
	if err != nil {
		return nil, err
	}

	// for each shard, apply the change
	log.Infof("Applying change on all shards")
	var applyErr error
	for i, shard := range shards {
		wg.Add(1)
		go func(i int, shard string) {
			defer wg.Done()

			_, err := wr.lockAndApplySchemaShard(ctx, shardInfos[i], preflight, keyspace, shard, shardInfos[i].MasterAlias, change, topo.TabletAlias{}, simple, force, waitSlaveTimeout)
			if err != nil {
				mu.Lock()
				applyErr = err
				mu.Unlock()
				return
			}
		}(i, shard)
	}
	wg.Wait()
	if applyErr != nil {
		return nil, applyErr
	}

	return &myproto.SchemaChangeResult{BeforeSchema: preflight.BeforeSchema, AfterSchema: preflight.AfterSchema}, nil
}

// CopySchemaShard copies the schema from a source tablet to the
// specified shard.  The schema is applied directly on the master of
// the destination shard, and is propogated to the replicas through
// binlogs.
func (wr *Wrangler) CopySchemaShard(ctx context.Context, srcTabletAlias topo.TabletAlias, tables, excludeTables []string, includeViews bool, keyspace, shard string) error {
	sd, err := wr.GetSchema(ctx, srcTabletAlias, tables, excludeTables, includeViews)
	if err != nil {
		return err
	}
	shardInfo, err := wr.ts.GetShard(keyspace, shard)
	if err != nil {
		return err
	}
	tabletInfo, err := wr.ts.GetTablet(shardInfo.MasterAlias)
	if err != nil {
		return err
	}
	createSql := sd.ToSQLStrings()

	for _, sqlLine := range createSql {
		err = wr.applySqlShard(ctx, tabletInfo, sqlLine)
		if err != nil {
			return err
		}
	}
	return nil
}

// applySqlShard applies a given SQL change on a given tablet alias. It allows executing arbitrary
// SQL statements, but doesn't return any results, so it's only useful for SQL statements
// that would be run for their effects (e.g., CREATE).
// It works by applying the SQL statement on the shard's master tablet with replication turned on.
// Thus it should be used only for changes that can be applies on a live instance without causing issues;
// it shouldn't be used for anything that will require a pivot.
// The SQL statement string is expected to have {{.DatabaseName}} in place of the actual db name.
func (wr *Wrangler) applySqlShard(ctx context.Context, tabletInfo *topo.TabletInfo, change string) error {
	filledChange, err := fillStringTemplate(change, map[string]string{"DatabaseName": tabletInfo.DbName()})
	if err != nil {
		return fmt.Errorf("fillStringTemplate failed: %v", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Need to make sure that we enable binlog, since we're only applying the statement on masters.
	_, err = wr.tmc.ExecuteFetch(ctx, tabletInfo, filledChange, 0, false, false)
	return err
}

// fillStringTemplate returns the string template filled
func fillStringTemplate(tmpl string, vars interface{}) (string, error) {
	myTemplate := template.Must(template.New("").Parse(tmpl))
	data := new(bytes.Buffer)
	if err := myTemplate.Execute(data, vars); err != nil {
		return "", err
	}
	return data.String(), nil
}
