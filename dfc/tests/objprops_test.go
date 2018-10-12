/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 *
 */
package dfc_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/NVIDIA/dfcpub/api"
	"github.com/NVIDIA/dfcpub/common"
	"github.com/NVIDIA/dfcpub/memsys"
	"github.com/NVIDIA/dfcpub/pkg/client"
)

func propsStats(t *testing.T, proxyURL string) (objChanged int64, bytesChanged int64) {
	stats := getClusterStats(httpclient, t, proxyURL)
	objChanged = 0
	bytesChanged = 0

	for _, v := range stats.Target {
		objChanged += v.Core.Tracker["vchange.n"].Value
		bytesChanged += v.Core.Tracker["vchange.size"].Value
	}

	return
}

func propsUpdateObjects(t *testing.T, proxyURL, bucket string, oldVersions map[string]string, msg *api.GetMsg,
	versionEnabled bool, isLocalBucket bool) (newVersions map[string]string) {
	newVersions = make(map[string]string, len(oldVersions))
	tlogf("Updating objects...\n")
	r, err := client.NewRandReader(int64(fileSize), true /* withHash */)
	if err != nil {
		t.Errorf("Failed to create reader: %v", err)
		t.Fail()
	}
	for fname := range oldVersions {
		err = client.Put(proxyURL, r, bucket, fname, !testing.Verbose())
		if err != nil {
			t.Errorf("Failed to put new data to object %s/%s, err: %v", bucket, fname, err)
			t.Fail()
		}
	}

	reslist := testListBucket(t, proxyURL, bucket, msg, 0)
	if reslist == nil {
		return
	}

	var (
		ver string
		ok  bool
	)
	for _, m := range reslist.Entries {
		if ver, ok = oldVersions[m.Name]; !ok {
			continue
		}
		tlogf("Object %s new version %s\n", m.Name, m.Version)
		newVersions[m.Name] = m.Version

		if !m.IsCached && !isLocalBucket {
			t.Errorf("Object %s/%s is not marked as cached one", bucket, m.Name)
		}
		if !versionEnabled {
			continue
		}

		if ver == m.Version {
			t.Errorf("Object %s/%s version has not changed", bucket, m.Name)
			t.Fail()
		} else if m.Version == "" {
			t.Errorf("Object %s/%s version is empty", bucket, m.Name)
			t.Fail()
		}
	}

	return
}

func propsReadObjects(t *testing.T, proxyURL, bucket string, filelist map[string]string) {
	versChanged, bytesChanged := propsStats(t, proxyURL)
	tlogf("Version mismatch stats before test. Objects: %d, bytes fetched: %d\n", versChanged, bytesChanged)

	for fname := range filelist {
		_, _, err := client.Get(proxyURL, bucket, fname, nil, nil, false, false)
		if err != nil {
			t.Errorf("Failed to read %s/%s, err: %v", bucket, fname, err)
			continue
		}
	}

	versChangedFinal, bytesChangedFinal := propsStats(t, proxyURL)
	tlogf("Version mismatch stats after test. Objects: %d, bytes fetched: %d\n", versChangedFinal, bytesChangedFinal)
	if versChanged != versChangedFinal || bytesChanged != bytesChangedFinal {
		t.Errorf("All objects must be retreived from the cache but cold get happened: %d times (%d bytes)",
			versChangedFinal-versChanged, bytesChangedFinal-bytesChanged)
		t.Fail()
	}
}

func propsEvict(t *testing.T, proxyURL, bucket string, objMap map[string]string, msg *api.GetMsg, versionEnabled bool) {
	// generate a object list to evict (evict 1/3 of total objects - random selection)
	toEvict := len(objMap) / 3
	if toEvict == 0 {
		toEvict = 1
	}
	toEvictList := make([]string, 0, toEvict)
	evictMap := make(map[string]bool, toEvict)
	tlogf("Evicting %v objects:\n", toEvict)

	for fname := range objMap {
		evictMap[fname] = true
		toEvictList = append(toEvictList, fname)
		tlogf("    %s/%s\n", bucket, fname)
		if len(toEvictList) >= toEvict {
			break
		}
	}

	err := client.EvictList(proxyURL, bucket, toEvictList, true, 0)
	if err != nil {
		t.Errorf("Failed to evict objects: %v\n", err)
		t.Fail()
	}

	tlogf("Reading object list...\n")

	// read a new object list and check that evicted objects do not have atime and iscached==false
	// version must be the same
	reslist := testListBucket(t, proxyURL, bucket, msg, 0)
	if reslist == nil {
		return
	}

	for _, m := range reslist.Entries {
		oldVersion, ok := objMap[m.Name]
		if !ok {
			continue
		}
		tlogf("%s/%s [%s] - iscached: [%v], atime [%v]\n", bucket, m.Name, m.Status, m.IsCached, m.Atime)

		// invalid object: rebalance leftover or uploaded directly to target
		if m.Status != "" {
			continue
		}

		if _, wasEvicted := evictMap[m.Name]; wasEvicted {
			if m.Atime != "" {
				t.Errorf("Evicted object %s/%s still has atime '%s'", bucket, m.Name, m.Atime)
				t.Fail()
			}
			if m.IsCached {
				t.Errorf("Evicted object %s/%s is still marked as cached one", bucket, m.Name)
				t.Fail()
			}
		}

		if !versionEnabled {
			continue
		}

		if m.Version == "" {
			t.Errorf("Object %s/%s version is empty", bucket, m.Name)
			t.Fail()
		} else if m.Version != oldVersion {
			t.Errorf("Object %s/%s version has changed from %s to %s", bucket, m.Name, oldVersion, m.Version)
			t.Fail()
		}
	}
}

func propsRecacheObjects(t *testing.T, proxyURL, bucket string, objs map[string]string, msg *api.GetMsg, versionEnabled bool) {
	tlogf("Refetching objects...\n")
	propsReadObjects(t, proxyURL, bucket, objs)
	tlogf("Checking objects properties after refetching...\n")
	reslist := testListBucket(t, proxyURL, bucket, msg, 0)
	if reslist == nil {
		t.Errorf("Unexpected erorr: no object in the bucket %s", bucket)
		t.Fail()
	}
	var (
		version string
		ok      bool
	)
	for _, m := range reslist.Entries {
		if version, ok = objs[m.Name]; !ok {
			continue
		}

		if !m.IsCached {
			t.Errorf("Object %s/%s is not marked as cached one", bucket, m.Name)
		}
		if m.Atime == "" {
			t.Errorf("Object %s/%s access time is empty", bucket, m.Name)
		}

		if !versionEnabled {
			continue
		}

		if m.Version == "" {
			t.Errorf("Failed to read object %s/%s version", bucket, m.Name)
			t.Fail()
		} else if version != m.Version {
			t.Errorf("Object %s/%s versions mismatch: old[%s], new[%s]", bucket, m.Name, version, m.Version)
			t.Fail()
		}
	}
}

func propsRebalance(t *testing.T, proxyURL, bucket string, objects map[string]string, msg *api.GetMsg, versionEnabled bool, isLocalBucket bool) {
	propsCleanupObjects(t, proxyURL, bucket, objects)

	smap := getClusterMap(t, proxyURL)
	l := len(smap.Tmap)
	if l < 2 {
		t.Skipf("Only %d targets found, need at least 2", l)
	}

	var (
		removedSid             string
		removedTargetDirectURL string
	)
	for sid, daemon := range smap.Tmap {
		removedSid = sid
		removedTargetDirectURL = daemon.PublicNet.DirectURL
		break
	}

	tlogf("Removing a target: %s\n", removedSid)
	err := client.UnregisterTarget(proxyURL, removedSid)
	checkFatal(err, t)
	smap, err = waitForPrimaryProxy(
		proxyURL,
		"target is gone",
		smap.Version, testing.Verbose(),
		len(smap.Pmap),
		len(smap.Tmap)-1,
	)
	checkFatal(err, t)

	tlogf("Target %s [%s] is removed\n", removedSid, removedTargetDirectURL)

	// rewrite objects and compare versions - they should change
	newobjs := propsUpdateObjects(t, proxyURL, bucket, objects, msg, versionEnabled, isLocalBucket)

	tlogf("Reregistering target...\n")
	err = client.RegisterTarget(removedSid, removedTargetDirectURL, smap)
	checkFatal(err, t)
	smap, err = waitForPrimaryProxy(
		proxyURL,
		"to join target back",
		smap.Version, testing.Verbose(),
		len(smap.Pmap),
		len(smap.Tmap)+1,
	)
	checkFatal(err, t)
	waitForRebalanceToComplete(t, proxyURL)

	tlogf("Reading file versions...\n")
	reslist := testListBucket(t, proxyURL, bucket, msg, 0)
	if reslist == nil {
		t.Errorf("Unexpected erorr: no object in the bucket %s", bucket)
		t.Fail()
	}
	var (
		version  string
		ok       bool
		objFound int
	)
	for _, m := range reslist.Entries {
		if version, ok = newobjs[m.Name]; !ok {
			continue
		}

		if m.Status != api.ObjStatusOK {
			continue
		}

		objFound++

		if !m.IsCached && !isLocalBucket {
			t.Errorf("Object %s/%s is not marked as cached one", bucket, m.Name)
		}
		if m.Atime == "" {
			t.Errorf("Object %s/%s access time is empty", bucket, m.Name)
		}

		if !versionEnabled {
			continue
		}

		tlogf("Object %s/%s, version before rebalance [%s], after [%s]\n", bucket, m.Name, version, m.Version)
		if version != m.Version {
			t.Errorf("Object %s/%s version mismatch: existing [%s], expected [%s]", bucket, m.Name, m.Version, version)
		}
	}

	if objFound != len(objects) {
		t.Errorf("The number of objects after rebalance differs for the number before it. Current: %d, expected %d", objFound, len(objects))
	}
}

func propsCleanupObjects(t *testing.T, proxyURL, bucket string, newVersions map[string]string) {
	errch := make(chan error, 100)
	wg := &sync.WaitGroup{}
	for objname := range newVersions {
		wg.Add(1)
		go client.Del(proxyURL, bucket, objname, wg, errch, !testing.Verbose())
	}
	wg.Wait()
	selectErr(errch, "delete", t, abortonerr)
	close(errch)
}

func propsTestCore(t *testing.T, versionEnabled bool, isLocalBucket bool) {
	const objCountToTest = 15
	const filesize = 1024 * 1024
	var (
		filesput   = make(chan string, objCountToTest)
		fileslist  = make(map[string]string, objCountToTest)
		errch      = make(chan error, objCountToTest)
		numPuts    = objCountToTest
		bucket     = clibucket
		versionDir = "versionid"
		sgl        *memsys.SGL
		proxyURL   = getPrimaryURL(t, proxyURLRO)
	)

	if usingSG {
		sgl = client.Mem2.NewSGL(filesize)
		defer sgl.Free()
	}

	// Create a few objects
	tlogf("Creating %d objects...\n", numPuts)
	ldir := LocalSrcDir + "/" + versionDir
	putRandomFiles(proxyURL, baseseed+110, filesize, int(numPuts), bucket, t, nil, errch, filesput,
		ldir, versionDir, true, sgl)
	selectErr(errch, "put", t, false)
	close(filesput)
	for fname := range filesput {
		if fname != "" {
			fileslist[versionDir+"/"+fname] = ""
		}
	}

	// Read object versions
	msg := &api.GetMsg{
		GetPrefix: versionDir,
		GetProps:  api.GetPropsVersion + ", " + api.GetPropsIsCached + ", " + api.GetPropsAtime + ", " + api.GetPropsStatus,
	}
	reslist := testListBucket(t, proxyURL, bucket, msg, 0)
	if reslist == nil {
		t.Errorf("Unexpected erorr: no object in the bucket %s", bucket)
		t.Fail()
		return
	}

	// PUT objects must have all properties set: atime, iscached, version
	for _, m := range reslist.Entries {
		if _, ok := fileslist[m.Name]; !ok {
			continue
		}
		tlogf("Initial version %s - %v\n", m.Name, m.Version)

		if !m.IsCached && !isLocalBucket {
			t.Errorf("Object %s/%s is not marked as cached one", bucket, m.Name)
		}

		if m.Atime == "" {
			t.Errorf("Object %s/%s access time is empty", bucket, m.Name)
		}

		if !versionEnabled {
			continue
		}

		if m.Version == "" {
			t.Error("Failed to read object version")
			t.Fail()
		} else {
			fileslist[m.Name] = m.Version
		}
	}

	// rewrite objects and compare versions - they should change
	newVersions := propsUpdateObjects(t, proxyURL, bucket, fileslist, msg, versionEnabled, isLocalBucket)
	if len(newVersions) != len(fileslist) {
		t.Errorf("Number of objects mismatch. Expected: %d objects, after update: %d", len(fileslist), len(newVersions))
	}

	// check that files are read from cache
	propsReadObjects(t, proxyURL, bucket, fileslist)

	if !isLocalBucket {
		// try to evict some files and check if they are gone
		propsEvict(t, proxyURL, bucket, newVersions, msg, versionEnabled)

		// read objects to put them to the cache. After that all objects must have iscached=true
		propsRecacheObjects(t, proxyURL, bucket, newVersions, msg, versionEnabled)
	}

	// test rebalance should keep object versions
	propsRebalance(t, proxyURL, bucket, newVersions, msg, versionEnabled, isLocalBucket)

	// cleanup
	propsCleanupObjects(t, proxyURL, bucket, newVersions)
}

func propsMainTest(t *testing.T, versioning string) {
	proxyURL := getPrimaryURL(t, proxyURLRO)
	chkVersion := true

	config := getConfig(proxyURL+common.URLPath(api.Version, api.Daemon), httpclient, t)
	versionCfg := config["version_config"].(map[string]interface{})
	oldChkVersion := versionCfg["validate_version_warm_get"].(bool)
	oldVersioning := versionCfg["versioning"].(string)
	if oldChkVersion != chkVersion {
		setConfig("validate_version_warm_get", fmt.Sprintf("%v", chkVersion), proxyURL+common.URLPath(api.Version, api.Cluster), httpclient, t)
	}
	if oldVersioning != versioning {
		setConfig("versioning", versioning, proxyURL+common.URLPath(api.Version, api.Cluster), httpclient, t)
	}
	created := createLocalBucketIfNotExists(t, proxyURL, clibucket)

	defer func() {
		// restore configuration
		if oldChkVersion != chkVersion {
			setConfig("validate_version_warm_get", fmt.Sprintf("%v", oldChkVersion), proxyURL+common.URLPath(api.Version, api.Cluster), httpclient, t)
		}
		if oldVersioning != versioning {
			setConfig("versioning", oldVersioning, proxyURL+common.URLPath(api.Version, api.Cluster), httpclient, t)
		}
		if created {
			if err := client.DestroyLocalBucket(proxyURL, clibucket); err != nil {
				t.Errorf("Failed to delete local bucket: %v", err)
			}
		}
	}()

	props, err := client.HeadBucket(proxyURL, clibucket)
	if err != nil {
		t.Fatalf("Could not execute HeadBucket Request: %v", err)
	}
	versionEnabled := props.Versioning != api.VersionNone
	isLocalBucket := props.CloudProvider == api.ProviderDFC
	propsTestCore(t, versionEnabled, isLocalBucket)
}

func TestObjPropsVersionEnabled(t *testing.T) {
	if testing.Short() {
		t.Skip("Long run only")
	}

	propsMainTest(t, api.VersionAll)
}

func TestObjPropsVersionDisabled(t *testing.T) {
	if testing.Short() {
		t.Skip("Long run only")
	}

	propsMainTest(t, api.VersionNone)
}
