// Package atime tracks object access times in the system while providing a number of performance enhancements.
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 */
package atime

import (
	"os"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/ios"
)

// ================================ Background ===========================================
// The atime (access time) module provides atime.Runner - a long running task with the
// purpose of updating object access times. The work is performed on a per local
// filesystem bases, via joggers (children). The atime.Runner's main responsibility
// is to dispatch requests to the corresponding mountpath jogger.
//
// API exposed to the rest of the code includes the following operations:
//   * Run      - to run
//   * Stop     - to stop
//   * Touch    - to request an access time update for a specified object
//   * Atime    - to request the most recent access time of a given object
// The remaining operations are private to the atime.Runner and used only internally.
//
// Each mountpath jogger has an access time map (in memory)
// that keeps track of object access times. Every so often atime.Runner
// calls joggers to flush access time maps.  Access times get flushed to
// the disk when the number of stored access times reaches a certain threshold
// and when:
//   * disk utilization is low, or
//   * access time map is filled over a certain point (watermark)
// This way, the atime.Runner and jogger operation will impact the
// datapath as little as possible.
//
// Important to keep in mind:
// - local filesystems **must** be configured with the noatime option (see fstab(5))
// - atime.Runner-cached timestamp takes precedence over the one stored by the filesystem
// - GET requests are the primary reason for the Touch (above) to be called
// ================================ Background ===========================================

const (
	chanCap      = 256
	jmapInitSize = 32
	LowWM        = 60
	HighWM       = 80
)

const (
	atimeTouch = iota
	atimeGet
)

var (
	atimeSyncTime = time.Minute * 2 // TODO: adjust at runtime
)

//
// API types
//
type (
	// atime.Runner gets and sets access times for a given object identified by its fqn.
	// atime.Runner implements the fsprunner interface where each mountpath has its own
	// jogger that manages requests on a per local filesystem basis.
	// atime.Runner will also periodically call its joggers
	// to flush files (read description above).
	Runner struct {
		cmn.NamedID
		requestCh  chan *atimeRequest // Requests for file access times or set access times
		stopCh     chan struct{}      // Control channel for stopping
		mpathReqCh chan fs.ChangeReq
		enabledCh  chan bool
		joggers    map[string]*jogger // mpath -> jogger
		mountpaths *fs.MountedFS
		ticker     *time.Ticker
	}
	// The Response object is used to return the access time of
	// an object in the atimemap and whether it actually existed in
	// the atimemap of the jogger it belongs to.
	Response struct {
		Ok         bool
		AccessTime time.Time
	}
)

//
// private types
//
type (
	// jogger handles a given specific mountpath/* as far as getting, setting, and flushing atimes
	jogger struct {
		mpathInfo *fs.MountpathInfo
		stopCh    chan struct{}        // Control channel for stopping
		atimemap  map[string]time.Time // maps fqn:atime key-value pairs
		getCh     chan *atimeRequest   // Requests for file access times
		setCh     chan *atimeRequest   // Requests to set access times
		flushCh   chan struct{}        // Request to flush atimes
	}

	// Each request to atime.Runner via its API is encapsulated in an
	// atimeRequest object. The responseCh is used to ensure each atime request gets its
	// corresponding response.
	// The accessTime field is used by Touch to set the atime of the requested object.
	// The mpath field is used by atime.Runner to determine which jogger to
	// dispatch the request to.
	atimeRequest struct {
		fqn         string
		accessTime  time.Time
		responseCh  chan *Response
		mpath       string
		requestType int // atimeTouch || atimeGet
	}
)

/*
 * implements fs.PathRunner interface
 */
var _ fs.PathRunner = &Runner{}

func (r *Runner) ReqAddMountpath(mpath string)     { r.mpathReqCh <- fs.MountpathAdd(mpath) }
func (r *Runner) ReqRemoveMountpath(mpath string)  { r.mpathReqCh <- fs.MountpathRem(mpath) }
func (r *Runner) ReqEnableMountpath(mpath string)  {}
func (r *Runner) ReqDisableMountpath(mpath string) {}

/*
 * subscribing to config changes
 */
var _ cmn.ConfigListener = &Runner{}

func (r *Runner) ConfigUpdate(oldConf, newConf *cmn.Config) {
	if oldConf.LRU.AtimerEnabled != newConf.LRU.AtimerEnabled {
		r.enabledCh <- newConf.LRU.AtimerEnabled
	}
}

//================================ atime.Runner ==========================================

func NewRunner(mountpaths *fs.MountedFS) (r *Runner) {
	return &Runner{
		stopCh:     make(chan struct{}, 4),
		mpathReqCh: make(chan fs.ChangeReq, 1),
		enabledCh:  make(chan bool, 1),
		mountpaths: mountpaths,
		requestCh:  make(chan *atimeRequest),
	}
}

func (r *Runner) init() {
	r.joggers = make(map[string]*jogger, 8)
	r.ticker = time.NewTicker(atimeSyncTime)
	availablePaths, disabledPaths := r.mountpaths.Get()
	for mpath := range availablePaths {
		r.addJogger(mpath)
	}
	for mpath := range disabledPaths {
		r.addJogger(mpath)
	}
}

func (r *Runner) term() {
	if r.ticker != nil {
		r.ticker.Stop()
		r.ticker = nil
	}
	// NOTE: not flushing cached atimes
	for _, jogger := range r.joggers {
		jogger.stop()
		jogger.atimemap = nil
	}
}

// Run initiates the work of the receiving atime.Runner
func (r *Runner) Run() error {
	glog.Infof("Starting %s", r.Getname())

	cmn.GCO.Subscribe(r)
	config := cmn.GCO.Get()
	if config.LRU.AtimerEnabled {
		goto run
	}
pause:
	glog.Warningf("Atimer %s is disabled and won't handle any atime updates", r.Getname())
	glog.Warning("including those that result from object migration and replication")
	for {
		select {
		case enabled := <-r.enabledCh:
			if enabled {
				goto run
			}
		case <-r.mpathReqCh:
		case request := <-r.requestCh:
			if request.requestType == atimeGet {
				request.responseCh <- &Response{}
			}
		case <-r.stopCh:
			r.term()
			return nil
		}
	}
run:
	r.init()
	glog.Infof("Running %s", r.Getname())
	for {
		select {
		case <-r.ticker.C:
			for _, jogger := range r.joggers {
				jogger.flushCh <- struct{}{}
			}
		case mpathRequest := <-r.mpathReqCh:
			switch mpathRequest.Action {
			case fs.Add:
				r.addJogger(mpathRequest.Path)
			case fs.Remove:
				r.removeJogger(mpathRequest.Path)
			}
		case request := <-r.requestCh:
			jogger, ok := r.joggers[request.mpath]
			if ok {
				if request.requestType == atimeTouch {
					jogger.setCh <- request
				} else {
					jogger.getCh <- request
				}
			} else if request.requestType == atimeGet { // invalid mpath
				request.responseCh <- &Response{}
			}
		case <-r.stopCh:
			r.term()
			return nil
		case enabled := <-r.enabledCh:
			if !enabled {
				r.term()
				goto pause
			}
		}
	}
}

// Stop terminates atime.Runner
func (r *Runner) Stop(err error) {
	glog.Infof("Stopping %s, err: %v", r.Getname(), err)
	r.stopCh <- struct{}{}
	close(r.stopCh)
}

// Touch requests an access time update for a given object. If not specified,
// the current time will be used. Note LRU must be enabled on the corresponding
// bucket.
func (r *Runner) Touch(mpath, fqn string, setTime ...time.Time) {
	var t time.Time
	if len(setTime) == 1 {
		t = setTime[0]
	} else {
		t = time.Now()
	}
	request := &atimeRequest{
		accessTime:  t,
		fqn:         fqn,
		mpath:       mpath,
		requestType: atimeTouch,
	}
	r.requestCh <- request
}

// atime requests the most recent access time of a given file.
// Note the atime method returns a channel. The caller of the function should
// block until it can receive from the channel an Response object, which
// indicates that the request has been fully processed. Note: caller of this
// method does not necessarily need to check if the bucket the object belongs
// to has LRU Enabled (a zero-valued Response will be returned)
// Note, the caller can optionally provide a customRespCh where the response will
// be written to. This reduces channel creation if atime is called repeatedly.
// Example usage:
//     Response := <-atimer.atime("/tmp/fqn123")
//     accessTime, ok := Response.AccessTime, Response.Ok
func (r *Runner) Atime(fqn, mpath string, customRespCh ...chan *Response) (responseCh chan *Response) {
	if len(customRespCh) == 1 {
		responseCh = customRespCh[0]
	} else {
		responseCh = make(chan *Response, 1)
	}
	if mpath == "" {
		mpathInfo, _ := r.mountpaths.Path2MpathInfo(fqn)
		if mpathInfo == nil {
			responseCh <- &Response{AccessTime: time.Time{}, Ok: false} // mpath does not exist
			return responseCh
		}
		mpath = mpathInfo.Path
	}
	request := &atimeRequest{
		responseCh:  responseCh,
		fqn:         fqn,
		mpath:       mpath,
		requestType: atimeGet,
	}
	r.requestCh <- request
	return responseCh
}

// convenience method to obtain atime from the (atime) cache or the file itself
func (r *Runner) GetAtime(fqn, mpath string, respCh chan *Response, useCache bool) (timeunix int64, err error) {
	var (
		atimeResp *Response
		atime     time.Time
		finfo     os.FileInfo
		ok        bool
	)
	if useCache {
		if respCh != nil {
			atimeResp = <-r.Atime(fqn, mpath, respCh)
		} else {
			atimeResp = <-r.Atime(fqn, mpath)
		}
		atime, ok = atimeResp.AccessTime, atimeResp.Ok
	}
	if !ok {
		finfo, err = os.Stat(fqn)
		if err == nil {
			atime = ios.GetATime(finfo)
		}
	}
	if err == nil {
		timeunix = atime.UnixNano()
	}
	return
}

//
// private methods
//

func (r *Runner) addJogger(mpath string) {
	if _, ok := r.joggers[mpath]; ok {
		glog.Warningf("Attempt to add already existing mountpath %q", mpath)
		return
	}
	mpathInfo, _ := r.mountpaths.Path2MpathInfo(mpath)
	if mpathInfo == nil {
		glog.Errorf("Attempt to add mountpath %q with no corresponding filesystem", mpath)
		return
	}
	jogger := r.newJogger(mpathInfo)
	r.joggers[mpath] = jogger
	go jogger.jog()
}

func (r *Runner) removeJogger(mpath string) {
	jogger, ok := r.joggers[mpath]
	if !ok {
		glog.Errorf("Invalid mountpath %q", mpath)
		return
	}
	jogger.stop()
	jogger.atimemap = nil
	delete(r.joggers, mpath)
}

//================================= jogger ===========================================

func (r *Runner) newJogger(mpathInfo *fs.MountpathInfo) *jogger {
	return &jogger{
		mpathInfo: mpathInfo,
		stopCh:    make(chan struct{}, 1),
		atimemap:  make(map[string]time.Time, jmapInitSize),
		getCh:     make(chan *atimeRequest),
		setCh:     make(chan *atimeRequest, chanCap),
		flushCh:   make(chan struct{}, 16),
	}
}

func (j *jogger) jog() {
	for {
		select {
		case request := <-j.getCh:
			accessTime, ok := j.atimemap[request.fqn]
			request.responseCh <- &Response{ok, accessTime}
		case request := <-j.setCh:
			j.atimemap[request.fqn] = request.accessTime
		case <-j.flushCh:
			j.flushAtimes()
		case <-j.stopCh:
			return
		}
	}
}

func (j *jogger) stop() {
	glog.Infof("Stopping jogger [%s]", j.mpathInfo.Path)
	j.stopCh <- struct{}{}
	close(j.stopCh)
}

// [throttle] num2flush estimates the number of timestamps that must be flushed
func (j *jogger) num2flush() (n int) {
	config := cmn.GCO.Get()
	maxlen := cmn.MaxI64(config.LRU.AtimeCacheMax, 1)
	lowlen := maxlen * LowWM / 100
	curlen := int64(len(j.atimemap))
	curpct := curlen * 100 / maxlen
	f := cmn.Ratio(HighWM, LowWM, curpct)
	n = int(float32(curlen-lowlen) * f)

	// TODO: handle the idle case in the slow path as part of the _TBD_ refactoring
	if n == 0 && j.mpathInfo.IsIdle(config) {
		n = int(curlen / 4)
	}
	return
}

// flush stores computed number of access times and removes the corresponding entries from the map.
func (j *jogger) flushAtimes() {
	var (
		mtime time.Time
		i     int
		n     = j.num2flush()
	)
	for fqn, atime := range j.atimemap {
		finfo, err := os.Stat(fqn)
		if err != nil {
			if !os.IsNotExist(err) {
				glog.Errorf("%s not-not-exists, fstat err: %v", fqn, err)
			}
		} else {
			mtime = finfo.ModTime()
			if err = os.Chtimes(fqn, atime, mtime); err != nil {
				glog.Errorf("Failed to touch %s, err: %v", fqn, err)
			}
		}
		delete(j.atimemap, fqn)
		i++
		if i >= n {
			break
		}
	}
}
