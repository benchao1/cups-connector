/*
Copyright 2015 Google Inc. All rights reserved.

Use of this source code is governed by a BSD-style
license that can be found in the LICENSE file or at
https://developers.google.com/open-source/licenses/bsd
*/
package manager

import (
	"cups-connector/cups"
	"cups-connector/gcp"
	"cups-connector/lib"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"
)

// Manages all interactions between CUPS and Google Cloud Print.
type PrinterManager struct {
	cups *cups.CUPS
	gcp  *gcp.GoogleCloudPrint

	// Do not mutate this map, only replace it with a new one. See syncPrinters().
	gcpPrintersByGCPID *lib.ConcurrentPrinterMap
	gcpJobPollQuit     chan bool
	printerPollQuit    chan bool
	downloadSemaphore  *lib.Semaphore
	gcpPrinterUpdates  chan string

	// Job stats are numbers reported to monitoring.
	jobStatsMutex sync.Mutex
	jobsDone      uint
	jobsError     uint

	// Jobs in flight are jobs that have been received, and are not
	// finished printing yet. Key is the GCP Job ID; value is meaningless.
	jobsInFlightMutex sync.Mutex
	jobsInFlight      map[string]bool

	cupsQueueSize     uint
	jobFullUsername   bool
	ignoreRawPrinters bool
	shareScope        string
}

func NewPrinterManager(cups *cups.CUPS, gcp *gcp.GoogleCloudPrint, printerPollInterval string, gcpMaxConcurrentDownload, cupsQueueSize uint, jobFullUsername, ignoreRawPrinters bool, shareScope string) (*PrinterManager, error) {
	// Get the GCP printer list.
	gcpPrinters, queuedJobsCount, xmppPingIntervalChanges, err := gcp.List()
	if err != nil {
		return nil, err
	}
	// Organize the GCP printers into a map.
	for i := range gcpPrinters {
		gcpPrinters[i].CUPSJobSemaphore = lib.NewSemaphore(cupsQueueSize)
	}
	gcpPrintersByGCPID := lib.NewConcurrentPrinterMap(gcpPrinters)

	// Update any pending XMPP ping interval changes.
	for gcpID := range xmppPingIntervalChanges {
		p, exists := gcpPrintersByGCPID.Get(gcpID)
		if !exists {
			// Ignore missing printers because this condition will resolve
			// itself as the connector continues to initialize.
			continue
		}
		if err = gcp.SetPrinterXMPPPingInterval(p); err != nil {
			return nil, err
		}
		glog.Infof("Printer %s XMPP ping interval changed to %s", p.Name, p.XMPPPingInterval.String())
	}

	// Set the connector XMPP ping interval the the min of all printers.
	var connectorXMPPPingInterval time.Duration = math.MaxInt64
	for _, p := range gcpPrintersByGCPID.GetAll() {
		if p.XMPPPingInterval < connectorXMPPPingInterval {
			connectorXMPPPingInterval = p.XMPPPingInterval
		}
	}
	gcp.SetConnectorXMPPPingInterval(connectorXMPPPingInterval)

	// Construct.
	pm := PrinterManager{
		cups: cups,
		gcp:  gcp,

		gcpPrintersByGCPID: gcpPrintersByGCPID,
		gcpJobPollQuit:     make(chan bool),
		printerPollQuit:    make(chan bool),
		downloadSemaphore:  lib.NewSemaphore(gcpMaxConcurrentDownload),
		gcpPrinterUpdates:  make(chan string),

		jobStatsMutex: sync.Mutex{},
		jobsDone:      0,
		jobsError:     0,

		jobsInFlightMutex: sync.Mutex{},
		jobsInFlight:      make(map[string]bool),

		cupsQueueSize:     cupsQueueSize,
		jobFullUsername:   jobFullUsername,
		ignoreRawPrinters: ignoreRawPrinters,
		shareScope:        shareScope,
	}

	// Sync once before returning, to make sure things are working.
	if err = pm.syncPrinters(); err != nil {
		return nil, err
	}

	ppi, err := time.ParseDuration(printerPollInterval)
	if err != nil {
		return nil, err
	}

	pm.syncPrintersPeriodically(ppi)
	pm.listenGCPJobs(queuedJobsCount)
	pm.listenGCPPrinterUpdates()

	return &pm, nil
}

func (pm *PrinterManager) Quit() {
	pm.printerPollQuit <- true
	<-pm.printerPollQuit
}

func (pm *PrinterManager) syncPrintersPeriodically(interval time.Duration) {
	go func() {
		t := time.NewTimer(interval)
		defer t.Stop()

		for {
			select {
			case <-t.C:
				if err := pm.syncPrinters(); err != nil {
					glog.Error(err)
				}
				t.Reset(interval)

			case gcpID := <-pm.gcpPrinterUpdates:
				p, err := pm.gcp.Printer(gcpID)
				if err != nil {
					glog.Error(err)
					continue
				}
				if err := pm.gcp.SetPrinterXMPPPingInterval(*p); err != nil {
					glog.Error(err)
					continue
				}
				glog.Infof("Printer %s XMPP ping interval changed to %s", p.Name, p.XMPPPingInterval.String())

			case <-pm.printerPollQuit:
				pm.printerPollQuit <- true
				break
			}
		}
	}()
}

func (pm *PrinterManager) syncPrinters() error {
	glog.Info("Synchronizing printers, stand by")

	cupsPrinters, err := pm.cups.GetPrinters()
	if err != nil {
		return fmt.Errorf("Sync failed while calling GetPrinters(): %s", err)
	}
	if pm.ignoreRawPrinters {
		cupsPrinters, _ = lib.FilterRawPrinters(cupsPrinters)
	}

	diffs := lib.DiffPrinters(cupsPrinters, pm.gcpPrintersByGCPID.GetAll())
	if diffs == nil {
		glog.Infof("Printers are already in sync; there are %d", len(cupsPrinters))
		return nil
	}

	ch := make(chan lib.Printer, len(diffs))
	for i := range diffs {
		go pm.applyDiff(&diffs[i], ch)
	}
	currentPrinters := make([]lib.Printer, 0, len(diffs))
	for _ = range diffs {
		p := <-ch
		if p.Name != "" {
			currentPrinters = append(currentPrinters, p)
		}
	}

	pm.gcpPrintersByGCPID.Refresh(currentPrinters)
	glog.Infof("Finished synchronizing %d printers", len(currentPrinters))

	return nil
}

func (pm *PrinterManager) applyDiff(diff *lib.PrinterDiff, ch chan<- lib.Printer) {
	switch diff.Operation {
	case lib.RegisterPrinter:
		ppd, err := pm.cups.GetPPD(diff.Printer.Name)
		if err != nil {
			glog.Errorf("Failed to call GetPPD() while registering printer %s: %s",
				diff.Printer.Name, err)
			break
		}
		if err := pm.gcp.Register(&diff.Printer, ppd); err != nil {
			glog.Errorf("Failed to register printer %s: %s", diff.Printer.Name, err)
			break
		}
		glog.Infof("Registered %s", diff.Printer.Name)

		if pm.gcp.CanShare() {
			if err := pm.gcp.Share(diff.Printer.GCPID, pm.shareScope); err != nil {
				glog.Errorf("Failed to share printer %s: %s", diff.Printer.Name, err)
			} else {
				glog.Infof("Shared %s", diff.Printer.Name)
			}
		}

		diff.Printer.CUPSJobSemaphore = lib.NewSemaphore(pm.cupsQueueSize)

		ch <- diff.Printer
		return

	case lib.UpdatePrinter:
		getPPD := func() (string, error) {
			return pm.cups.GetPPD(diff.Printer.Name)
		}

		if err := pm.gcp.Update(diff, getPPD); err != nil {
			glog.Errorf("Failed to update a printer: %s", err)
		} else {
			glog.Infof("Updated %s", diff.Printer.Name)
		}

		ch <- diff.Printer
		return

	case lib.DeletePrinter:
		pm.cups.RemoveCachedPPD(diff.Printer.Name)
		if err := pm.gcp.Delete(diff.Printer.GCPID); err != nil {
			glog.Errorf("Failed to delete a printer %s: %s", diff.Printer.GCPID, err)
			break
		}
		glog.Infof("Deleted %s", diff.Printer.Name)

	case lib.NoChangeToPrinter:
		glog.Infof("No change to %s", diff.Printer.Name)
		ch <- diff.Printer
		return
	}

	ch <- lib.Printer{}
}

func (pm *PrinterManager) listenGCPJobs(queuedJobsCount map[string]uint) {
	ch := make(chan *lib.Job)

	for gcpID := range queuedJobsCount {
		go func() {
			jobs, err := pm.gcp.Fetch(gcpID)
			if err != nil {
				glog.Warningf("Error fetching print jobs: %s", err)
				return
			}

			if len(jobs) > 0 {
				glog.Infof("Fetched %d waiting print jobs for printer %s", len(jobs), gcpID)
			}
			for i := range jobs {
				ch <- &jobs[i]
			}
		}()
	}

	go func() {
		for {
			jobs, err := pm.gcp.NextJobBatch()
			if err != nil {
				glog.Errorf("Failed to fetch job batch: %s", err)

			} else {
				for i := range jobs {
					ch <- &jobs[i]
				}
			}
		}
	}()

	go func() {
		for {
			select {
			case job := <-ch:
				go pm.processJob(job)
			case <-pm.gcpJobPollQuit:
				pm.gcpJobPollQuit <- true
				return
			}
		}
	}()
}

func (pm *PrinterManager) listenGCPPrinterUpdates() {
	go func() {
		for {
			gcpID := pm.gcp.NextPrinterWithUpdates()
			pm.gcpPrinterUpdates <- gcpID
		}
	}()
}

func (pm *PrinterManager) incrementJobsProcessed(success bool) {
	pm.jobStatsMutex.Lock()
	defer pm.jobStatsMutex.Unlock()

	if success {
		pm.jobsDone += 1
	} else {
		pm.jobsError += 1
	}
}

// addInFlightJob adds a job GCP ID to the in flight set.
//
// Returns true if the job GCP ID was added, false if it already exists.
func (pm *PrinterManager) addInFlightJob(gcpJobID string) bool {
	pm.jobsInFlightMutex.Lock()
	defer pm.jobsInFlightMutex.Unlock()

	if pm.jobsInFlight[gcpJobID] {
		return false
	}

	pm.jobsInFlight[gcpJobID] = true

	return true
}

// deleteInFlightJob deletes a job from the in flight set.
func (pm *PrinterManager) deleteInFlightJob(gcpID string) {
	pm.jobsInFlightMutex.Lock()
	defer pm.jobsInFlightMutex.Unlock()

	delete(pm.jobsInFlight, gcpID)
}

// assembleJob prepares for printing a job by fetching the job's printer,
// ticket (aka options), and the job's PDF (what we're printing)
//
// The caller is responsible to remove the returned PDF file.
//
// Errors are returned as a string (last return value), for reporting
// to GCP and local logging.
func (pm *PrinterManager) assembleJob(job *lib.Job) (lib.Printer, map[string]string, *os.File, string, lib.GCPJobStateCause) {
	printer, exists := pm.gcpPrintersByGCPID.Get(job.GCPPrinterID)
	if !exists {
		return lib.Printer{}, nil, nil,
			fmt.Sprintf("Failed to find GCP printer %s for job %s", job.GCPPrinterID, job.GCPJobID),
			lib.GCPJobOther
	}

	options, err := pm.gcp.Ticket(job.GCPJobID)
	if err != nil {
		return lib.Printer{}, nil, nil,
			fmt.Sprintf("Failed to get a ticket for job %s: %s", job.GCPJobID, err),
			lib.GCPJobInvalidTicket
	}

	pdfFile, err := cups.CreateTempFile()
	if err != nil {
		return lib.Printer{}, nil, nil,
			fmt.Sprintf("Failed to create a temporary file for job %s: %s", job.GCPJobID, err),
			lib.GCPJobOther
	}

	pm.downloadSemaphore.Acquire()
	t := time.Now()
	// Do not check err until semaphore is released and timer is stopped.
	err = pm.gcp.Download(pdfFile, job.FileURL)
	dt := time.Since(t)
	pm.downloadSemaphore.Release()
	if err != nil {
		// Clean up this temporary file so the caller doesn't need extra logic.
		os.Remove(pdfFile.Name())
		return lib.Printer{}, nil, nil,
			fmt.Sprintf("Failed to download PDF for job %s: %s", job.GCPJobID, err),
			lib.GCPJobPrintFailure
	}

	glog.Infof("Downloaded job %s in %s", job.GCPJobID, dt.String())
	pdfFile.Close()

	return printer, options, pdfFile, "", 100
}

// processJob performs these steps:
//
// 1) Assembles the job resources (printer, ticket, PDF)
// 2) Creates a new job in CUPS.
// 3) Follows up with the job state until done or error.
// 4) Deletes temporary file.
//
// Nothing is returned; intended for use as goroutine.
func (pm *PrinterManager) processJob(job *lib.Job) {
	if !pm.addInFlightJob(job.GCPJobID) {
		// This print job was already received. We probably received it
		// again because the first instance is still queued (ie not
		// IN_PROGRESS). That's OK, just throw away the second instance.
		return
	}
	defer pm.deleteInFlightJob(job.GCPJobID)

	glog.Infof("Received job %s", job.GCPJobID)

	printer, options, pdfFile, message, gcpJobStateCause := pm.assembleJob(job)
	if message != "" {
		pm.incrementJobsProcessed(false)
		glog.Error(message)
		if err := pm.gcp.Control(job.GCPJobID, lib.GCPJobAborted, gcpJobStateCause, 0); err != nil {
			glog.Error(err)
		}
		return
	}
	defer os.Remove(pdfFile.Name())

	ownerID := job.OwnerID
	if !pm.jobFullUsername {
		ownerID = strings.Split(ownerID, "@")[0]
	}

	printer.CUPSJobSemaphore.Acquire()
	defer printer.CUPSJobSemaphore.Release()

	jobTitle := fmt.Sprintf("gcp:%s %s", job.GCPJobID, job.Title)
	if len(jobTitle) > 255 {
		jobTitle = jobTitle[:255]
	}

	cupsJobID, err := pm.cups.Print(printer.Name, pdfFile.Name(), jobTitle, ownerID, options)
	if err != nil {
		pm.incrementJobsProcessed(false)
		message = fmt.Sprintf("Failed to send job %s to CUPS: %s", job.GCPJobID, err)
		glog.Error(message)
		if err := pm.gcp.Control(job.GCPJobID, lib.GCPJobAborted, lib.GCPJobPrintFailure, 0); err != nil {
			glog.Error(err)
		}
		return
	}

	glog.Infof("Submitted GCP job %s as CUPS job %d", job.GCPJobID, cupsJobID)

	pm.followJob(job, cupsJobID)
}

// followJob polls a CUPS job state to update the GCP job state and
// returns when the job state is DONE or ERROR.
//
// Nothing is returned, as all errors are reported and logged from
// this function.
func (pm *PrinterManager) followJob(job *lib.Job, cupsJobID uint32) {
	var cupsState lib.CUPSJobState
	var gcpState lib.GCPJobState
	var pages uint32

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for _ = range ticker.C {
		latestCUPSState, latestPages, err := pm.cups.GetJobState(cupsJobID)
		if err != nil {
			glog.Warningf("Failed to get state of CUPS job %d: %s", cupsJobID, err)
			if err := pm.gcp.Control(job.GCPJobID, lib.GCPJobAborted, lib.GCPJobOther, pages); err != nil {
				glog.Error(err)
			}
			pm.incrementJobsProcessed(false)
			break
		}

		if latestCUPSState != cupsState || latestPages != pages {
			cupsState = latestCUPSState
			var gcpCause lib.GCPJobStateCause
			gcpState, gcpCause = latestCUPSState.GCPJobState()
			pages = latestPages
			if err = pm.gcp.Control(job.GCPJobID, gcpState, gcpCause, pages); err != nil {
				glog.Error(err)
			}
			glog.Infof("Job %s state is now: %s/%s", job.GCPJobID, cupsState, gcpState)
		}

		if gcpState != lib.GCPJobInProgress {
			if gcpState == lib.GCPJobDone {
				pm.incrementJobsProcessed(true)
			} else {
				pm.incrementJobsProcessed(false)
			}
			break
		}
	}
}

// GetJobStats returns information that is useful for monitoring
// the connector.
func (pm *PrinterManager) GetJobStats() (uint, uint, uint, error) {
	var processing uint

	for _, printer := range pm.gcpPrintersByGCPID.GetAll() {
		processing += printer.CUPSJobSemaphore.Count()
	}

	pm.jobStatsMutex.Lock()
	defer pm.jobStatsMutex.Unlock()

	return pm.jobsDone, pm.jobsError, processing, nil
}
