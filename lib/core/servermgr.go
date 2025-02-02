package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"path"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MG-RAST/AWE/lib/conf"
	"github.com/MG-RAST/AWE/lib/core/cwl"
	e "github.com/MG-RAST/AWE/lib/errors"
	"github.com/MG-RAST/AWE/lib/logger"
	"github.com/MG-RAST/AWE/lib/logger/event"
	"github.com/MG-RAST/AWE/lib/user"
	shock "github.com/MG-RAST/go-shock-client"
	uuid "github.com/MG-RAST/golib/go-uuid/uuid"
	"github.com/davecgh/go-spew/spew"
	"github.com/robertkrimen/otto"
	"gopkg.in/mgo.v2/bson"
)

type jQueueShow struct {
	Active  map[string]*JobPerf `bson:"active" json:"active"`
	Suspend map[string]bool     `bson:"suspend" json:"suspend"`
}

// ServerMgr _
type ServerMgr struct {
	CQMgr
	queueLock      sync.Mutex //only update one at a time
	lastUpdate     time.Time
	lastUpdateLock sync.RWMutex
	TaskMap        TaskMap
	ajLock         sync.RWMutex
	actJobs        map[string]*JobPerf
}

// NewServerMgr _
func NewServerMgr() *ServerMgr {
	return &ServerMgr{
		CQMgr: CQMgr{
			clientMap:    *NewClientMap(),
			workQueue:    NewWorkQueue(),
			suspendQueue: false,

			coReq:    make(chan CheckoutRequest, conf.COREQ_LENGTH), // number of clients that wait in queue to get a workunit. If queue is full, other client will be rejected and have to come back later again
			feedback: make(chan Notice),
			coSem:    make(chan int, 1), //non-blocking buffered channel

		},
		lastUpdate: time.Now().Add(time.Second * -30),
		TaskMap:    *NewTaskMap(),
		actJobs:    map[string]*JobPerf{},
	}
}

//--------mgr methods-------

// Lock _
func (qm *ServerMgr) Lock() {}

// Unlock _
func (qm *ServerMgr) Unlock() {}

// RLock _
func (qm *ServerMgr) RLock() {}

// RUnlock _
func (qm *ServerMgr) RUnlock() {}

// UpdateQueueLoop _
func (qm *ServerMgr) UpdateQueueLoop() {
	// TODO this may not be dynamic enough for small amounts of workunits, as they always have to wait

	var err error
	for {

		err = qm.updateWorkflowInstancesMap()
		if err != nil {
			logger.Error("(UpdateQueueLoop) updateWorkflowInstancesMap() returned: %s", err.Error())
			err = nil
		}

		// *** task ***

		logTimes := false
		start := time.Now()
		err = qm.updateQueue(logTimes)
		if err != nil {
			logger.Error("(UpdateQueueLoop) updateQueue returned: %s", err.Error())
			err = nil
		}
		elapsed := time.Since(start)        // type Duration
		elapsedSeconds := elapsed.Seconds() // type float64

		var sleeptime time.Duration
		if elapsedSeconds <= 1 {
			sleeptime = 1 * time.Second // wait at least 1 second
		} else if elapsedSeconds > 1 && elapsedSeconds < 30 {
			sleeptime = elapsed
		} else {
			sleeptime = 30 * time.Second // wait at most 30 seconds
		}

		logger.Debug(0, "(UpdateQueueLoop) elapsed: %s (sleeping for %s)", elapsed, sleeptime)

		time.Sleep(sleeptime)

	}
}

// IsWIReady _
func (qm *ServerMgr) IsWIReady(job *Job, wi *WorkflowInstance) (ready bool, reason string, err error) {

	ready = false
	reason = "unknown"

	logger.Debug(3, "(IsWIReady) start")
	if wi.Inputs != nil {
		logger.Debug(3, "(IsWIReady) wi.Inputs != nil")
		ready = true
		return
	}

	cwlWorkflow := wi.Workflow
	if cwlWorkflow == nil {
		err = fmt.Errorf("IsWIReady wi.Workflow==nil")
		return
	}

	//fmt.Println("cwlWorkflow.Inputs:")
	//spew.Dump(cwlWorkflow.Inputs)

	//fmt.Println("wi.LocalID: " + wi.LocalID)
	parentWorkflowInstanceName := path.Dir(wi.LocalID)

	if parentWorkflowInstanceName == "." {
		logger.Debug(3, "(IsWIReady) parentWorkflowInstanceName == .  (wi.LocalID=%s)", wi.LocalID)
		ready = true
		return
	}
	logger.Debug(3, "(IsWIReady) non-main")

	//fmt.Println("parentWorkflowInstanceName: " + parentWorkflowInstanceName)

	var parentWorkflowInstance *WorkflowInstance
	var ok bool
	parentWorkflowInstance, ok, err = job.GetWorkflowInstance(parentWorkflowInstanceName, true)
	if err != nil {
		err = fmt.Errorf("(IsWIReady) job.GetWorkflowInstance returned: %s", err.Error())
		return
	}
	if !ok {
		err = fmt.Errorf("(IsWIReady) parent workflowInstance %s not found", parentWorkflowInstanceName)
		return
	}

	_ = parentWorkflowInstance
	_ = ok

	var parentStep *cwl.WorkflowStep
	parentStep, err = wi.GetWorkflowStep(job)
	if err != nil {
		err = fmt.Errorf("(IsWIReady) wi.GetWorkflowStep returned: %s", err.Error())
		return
	}

	if parentStep == nil {
		err = fmt.Errorf("(IsWIReady)  wi.WorkflowStep==nil (wi.LocalID=%s)", wi.LocalID)
		return
	}

	parentWorkflowInputMap := parentWorkflowInstance.Inputs.GetMap()

	context := job.WorkflowContext
	var generators []string
	ready, reason, generators, err = qm.areSourceGeneratorsReady(parentStep, job, parentWorkflowInstance)
	if err != nil {
		err = fmt.Errorf("(IsWIReady) areSourceGeneratorsReady returned: %s (generators: %s)", err.Error(), strings.Join(generators, ","))
		return
	}

	if !ready {
		return
	}
	logger.Debug(3, "(IsWIReady) areSourceGeneratorsReady returned generators list: %s (%s)", strings.Join(generators, ","), parentWorkflowInstanceName)

	// err = qm.GetDependencies(job, parentWorkflowInstance, parentWorkflowInputMap, step, context)
	// if err != nil {
	// 	err = fmt.Errorf("(IsWIReady) GetDependencies returned: %s", err.Error())
	// }

	// panic("done")

	//var workunit_input_map map[string]cwl.CWLType
	var parentStepInputs []*cwl.WorkflowStepInput
	parentStepInputs, err = parentStep.GetStepInputs()
	if err != nil {
		err = fmt.Errorf("(IsWIReady) parentStep.GetStepInputs returned: %s", err.Error())
		return
	}

	var workunitInputMap cwl.JobDocMap
	workunitInputMap, ok, reason, err = qm.GetStepInputObjects(job, parentWorkflowInstance, parentWorkflowInputMap, parentStepInputs, context, "IsWIReady")
	if err != nil {
		err = fmt.Errorf("(IsWIReady) GetStepInputObjects returned: %s", err.Error())
		return
	}

	if !ok {
		ready = false
		return
	}
	//fmt.Println("workunit_input_map:")
	//spew.Dump(workunit_input_map)

	var workunitInputArray cwl.Job_document
	workunitInputArray, err = workunitInputMap.GetArray()

	wi.Inputs = workunitInputArray
	logger.Debug(3, "(IsWIReady) completed")
	ready = true

	return

}

func (qm *ServerMgr) updateWorkflowInstancesMapTask(workflowInstance *WorkflowInstance) (err error) {

	wiLocalID := workflowInstance.LocalID
	parentWiUUID := workflowInstance.ID
	wiState, _ := workflowInstance.GetState(true)

	logger.Debug(3, "(updateWorkflowInstancesMapTask) start: %s state: %s", workflowInstance.LocalID, wiState)

	if wiState == WIStateSuspended {
		return
	}

	if wiState == WIStatePending {

		if len(workflowInstance.Tasks) > 0 {
			spew.Dump(workflowInstance.Tasks)
			err = fmt.Errorf("(updateWorkflowInstancesMapTask) A) WI claims to be in state pending, but already has %d tasks", len(workflowInstance.Tasks))
			return
		}

		if workflowInstance.ProcessType == ProcessTypeScatter {
			logger.Debug(3, "(updateWorkflowInstancesMapTask) start: %s returning, is ProcessTypeScatter", workflowInstance.LocalID)
			return
		}

		jobid := workflowInstance.JobID
		//workflow_def_str := wi.Workflow_Definition

		var job *Job
		job, err = GetJob(jobid)
		if err != nil {
			err = fmt.Errorf("(updateWorkflowInstancesMapTask) GetJob failed: %s", err.Error())
			return

		}

		context := job.WorkflowContext

		// get workflow

		var cwlWorkflow *cwl.Workflow
		if workflowInstance.Workflow == nil {

			cwlWorkflow, err = workflowInstance.GetWorkflow(context)
			if err != nil {
				err = fmt.Errorf("(updateWorkflowInstancesMapTask) GetWorkflow failed: %s", err.Error())
				return
			}
			if cwlWorkflow == nil {
				err = fmt.Errorf("(updateWorkflowInstancesMapTask) a) cwlWorkflow == nil")
				return
			}
			workflowInstance.Workflow = cwlWorkflow
		} else {
			cwlWorkflow = workflowInstance.Workflow
		}
		if cwlWorkflow == nil {
			err = fmt.Errorf("(updateWorkflowInstancesMapTask) b) cwlWorkflow == nil")
			return
		}

		//if cwlWorkflow.Steps == nil {
		//	err = fmt.Errorf("(updateWorkflowInstancesMapTask) cwlWorkflow.Steps == nil")
		//	return
		//}

		// check if workflowInstance is ready

		var ready bool
		var reason string
		ready, reason, err = qm.IsWIReady(job, workflowInstance)
		if err != nil {
			err = fmt.Errorf("(updateWorkflowInstancesMapTask) qm.IsWIReady returned: %s", err.Error())
			return
		}

		if !ready {
			logger.Debug(3, "(updateWorkflowInstancesMapTask) Wi is not ready (%s), reason: %s", workflowInstance.LocalID, reason)
			return
		}

		// for each step create Task or Subworkflow

		if len(workflowInstance.Tasks) > 0 {
			spew.Dump(workflowInstance.Tasks)
			err = fmt.Errorf("(updateWorkflowInstancesMapTask) B) WI claims to be in state pending, but already has %d tasks", len(workflowInstance.Tasks))
			return
		}

		//subworkflow_str := []string{}
		noSteps := false

		if cwlWorkflow.Steps != nil {

			logger.Debug(3, "(updateWorkflowInstancesMapTask) %s len(cwlWorkflow.Steps)=%d", workflowInstance.LocalID, len(cwlWorkflow.Steps))

			for i := range cwlWorkflow.Steps {

				step := &cwlWorkflow.Steps[i]

				if step == nil {
					err = fmt.Errorf("(updateWorkflowInstancesMapTask) step == nil ")
					return
				}

				stepnameBase := path.Base(step.ID)

				instanceStepName := path.Join(wiLocalID, stepnameBase)

				//steps_str = append(steps_str, wi_localwiLocalID_id+"/"+stepname_base)
				var process interface{}
				process, _, err = step.GetProcess(context)
				if err != nil {
					err = fmt.Errorf("(updateWorkflowInstancesMapTask) step.GetProcess returned: %s", err.Error())
					return
				}

				switch process.(type) {
				case *cwl.CommandLineTool, *cwl.ExpressionTool:
					// creates a Task

					logger.Debug(3, "(updateWorkflowInstancesMapTask) Creating (CommandLine/Expression) %s", wiLocalID+"/"+stepnameBase)
					var aweTask *Task
					aweTask, err = NewTask(job, parentWiUUID, wiLocalID, stepnameBase)
					if err != nil {
						err = fmt.Errorf("(updateWorkflowInstancesMapTask) NewTask returned: %s", err.Error())
						return
					}

					if len(step.Scatter) > 0 {
						logger.Debug(3, "(updateWorkflowInstancesMapTask) detected ProcessTypeScatter")
						aweTask.ProcessType = ProcessTypeScatter
					} else {
						logger.Debug(3, "(updateWorkflowInstancesMapTask) detected ProcessTypeNormal")
						aweTask.ProcessType = ProcessTypeNormal
					}

					aweTask.WorkflowStep = step
					aweTask.WorkflowStepID = step.ID

					_, err = aweTask.Init(job, jobid)
					if err != nil {
						err = fmt.Errorf("(updateWorkflowInstancesMapTask) aweTask.Init returned: %s", err.Error())
						return
					}

					logger.Debug(3, "(updateWorkflowInstancesMapTask) adding %s to workflowInstance", aweTask.TaskName)
					err = workflowInstance.AddTask(job, aweTask, DbSyncTrue, true)
					if err != nil {
						err = fmt.Errorf("(updateWorkflowInstancesMapTask) workflowInstance.AddTask returned: %s", err.Error())
						return
					}

					err = qm.TaskMap.Add(aweTask, "updateWorkflowInstancesMapTask")
					if err != nil {
						err = fmt.Errorf("(updateWorkflowInstancesMapTask) qm.TaskMap.Add returned: %s", err.Error())
						return
					}

					//panic("got CommandLineTool")
					// create Task

				//case *cwl.ExpressionTool:
				//	fmt.Println("(updateWorkflowInstancesMapTask) ExpressionTool")
				//create Task

				case *cwl.Workflow:
					// creates a WorkflowInstance

					//subworkflow_str = append(subworkflow_str, wi_local_id+"/"+stepname_base)
					// create new WorkflowInstance

					subworkflow, ok := process.(*cwl.Workflow)
					if !ok {
						err = fmt.Errorf("(updateWorkflowInstancesMapTask) cannot cast to *cwl.Workflow")
						return
					}

					subworkflowID := subworkflow.GetID()

					fmt.Printf("(updateWorkflowInstancesMapTask) Creating Workflow %s\n", subworkflowID)

					newWIName := instanceStepName

					// TODO assign inputs
					//var workflow_inputs cwl.Job_document

					//panic("creating new subworkflow " + newWIName)
					var new_wi *WorkflowInstance
					new_wi, err = NewWorkflowInstance(newWIName, jobid, subworkflowID, job, workflowInstance.LocalID)
					if err != nil {
						err = fmt.Errorf("(updateWorkflowInstancesMapTask) NewWorkflowInstance returned: %s", err.Error())
						return
					}

					new_wi.Workflow = subworkflow
					new_wi.WorkflowStep = step
					new_wi.WorkflowStepID = step.ID

					if len(step.Scatter) > 0 {
						logger.Debug(3, "(updateWorkflowInstancesMapTask) detected scatter")
						//new_wi.IsScatter = true
						err = new_wi.SetProcessType(ProcessTypeScatter, false, false)
						if err != nil {
							err = fmt.Errorf("(updateWorkflowInstancesMapTask) new_wi.SetProcessType returned: %s", err.Error())
							return
						}

						if workflowInstance.Inputs == nil {
							err = fmt.Errorf("(updateWorkflowInstancesMapTask) workflowInstance.Inputs == nil")
							return
						}

					} else {
						err = new_wi.SetProcessType(ProcessTypeNormal, false, false)
						if err != nil {
							err = fmt.Errorf("(updateWorkflowInstancesMapTask) new_wi.SetProcessType returned: %s", err.Error())
							return
						}
					}

					//new_wi.SetState(WIStatePending, "db_sync_no", false) // updateWorkflowInstancesMapTask
					//AddWorkflowInstance sets steat to WIStatePending
					err = job.AddWorkflowInstance(new_wi, DbSyncTrue, true) // updateWorkflowInstancesMapTask
					if err != nil {
						err = fmt.Errorf("(updateWorkflowInstancesMapTask) job.AddWorkflowInstance returned: %s", err.Error())
						return
					}

					if len(step.Scatter) > 0 {

						logger.Debug(3, "(updateWorkflowInstancesMapTask) Step has Scatter: %s (wiLocalID: %s)", step.ID, wiLocalID)

						workflowInputMap := workflowInstance.Inputs.GetMap()

						if new_wi.WorkflowStep == nil {
							err = fmt.Errorf("(updateWorkflowInstancesMapTask) new_wi.WorkflowStep == nil")
							return
						}
						fmt.Println("new_wi, before")
						spew.Dump(new_wi)

						//job *Job, workflowInputMap cwl.JobDocMap
						_, err = qm.processInstanceEnQueueScatter(workflowInstance, new_wi, job, workflowInputMap)
						if err != nil {
							fmt.Println("new_wi, after")
							spew.Dump(new_wi)
							fmt.Println("dying because of: " + err.Error())
							//panic("arrrgghh")
							err = fmt.Errorf("(updateWorkflowInstancesMapTask) processInstanceEnQueueScatter returned: %s", err.Error())
							return
						}

					}

					newWIUniqueID, _ := new_wi.GetID(true)

					err = GlobalWorkflowInstanceMap.Add(newWIUniqueID, new_wi)
					if err != nil {
						err = fmt.Errorf("(updateWorkflowInstancesMapTask) GlobalWorkflowInstanceMap.Add returned: %s", err.Error())
						return
					}

					err = workflowInstance.AddSubworkflow(job, new_wi.LocalID, true)
					if err != nil {
						err = fmt.Errorf("(updateWorkflowInstancesMapTask) wi.AddSubworkflow returned: %s", err.Error())
						return
					}

				default:
					err = fmt.Errorf("(updateWorkflowInstancesMapTask) type unknown: %s", reflect.TypeOf(process))
					return
				}

				//spew.Dump(cwlWorkflow.Steps[i])

			}
		} else {
			//panic("not steps, you sure ?")
			noSteps = true
		}
		//wi.Subworkflows = subworkflow_str

		// pending -> ready
		err = workflowInstance.SetState(WIStateReady, true, "updateWorkflowInstancesMapTask")
		if err != nil {
			err = fmt.Errorf("(updateWorkflowInstancesMapTask) wi.SetState returned: %s", err.Error())
			return
		}
		//spew.Dump(wi)
		//fmt.Printf("SHOULD BE READY NOW (%p)\n", wi)

		if workflowInstance.Inputs != nil && len(workflowInstance.Inputs) > 0 {
			//panic("found something...")

			var tasks []*Task
			tasks, err = workflowInstance.GetTasks(true)
			if err != nil {
				return
			}

			err = qm.EnqueueTasks(tasks)
			if err != nil {
				err = fmt.Errorf("(updateWorkflowInstancesMapTask) EnqueueTasks returned: %s", err.Error())

				return
			}
		}

		err = workflowInstance.SetState(WIStateQueued, true, "updateWorkflowInstancesMapTask")
		if err != nil {
			err = fmt.Errorf("(updateWorkflowInstancesMapTask) SetState returned: %s", err.Error())
			return
		}

		// update job state
		if wiLocalID == job.Entrypoint {

			jobState, _ := job.GetState(true)
			if jobState == JOB_STAT_INIT {
				err = job.SetState(JOB_STAT_QUEUED, []string{JOB_STAT_INIT})
				if err != nil {
					err = fmt.Errorf("(updateWorkflowInstancesMapTask) job.SetState returned: %s", err.Error())
					return
				}
			}

		}

		if noSteps {

			var reason string
			var ok bool
			ok, reason, err = qm.completeSubworkflow(job, workflowInstance) // taskCompleted
			if err != nil {
				err = fmt.Errorf("(updateWorkflowInstancesMapTask) completeSubworkflow returned: %s", err.Error())
				return
			}
			if !ok {
				err = fmt.Errorf("(updateWorkflowInstancesMapTask) completeSubworkflow not ok, reason: %s", reason)
				return
			}
			// err = qm.finalizeJob(job)
			// if err != nil {
			// 	err = fmt.Errorf("(taskCompleted) qm.finalizeJob returned: %s", err.Error())
			// 	return
			// }
		}
	}

	return
}

func (qm *ServerMgr) updateWorkflowInstancesMap() (err error) {

	var wis []*WorkflowInstance
	wis, err = GlobalWorkflowInstanceMap.GetWorkflowInstances()
	if err != nil {
		err = fmt.Errorf("(updateWorkflowInstancesMap) ")
		return
	}

	var lastError error

	for i := range wis {

		wi := wis[i]
		fmt.Printf(" *** WI *** id=%s, state=%s processType=%s\n", wi.LocalID, wi.State, wi.ProcessType)
	}

	errorCount := 0
	for i := range wis {

		wi := wis[i]

		err = qm.updateWorkflowInstancesMapTask(wi)

		if err != nil {
			lastError = err
			errorCount++

			errorMessage := err.Error()
			err = nil

			jobID := wi.JobID

			jerror := &JobError{

				ServerNotes: fmt.Sprintf("WorkflowInstance failed: %s", errorMessage),
				Status:      JOB_STAT_SUSPEND,
			}
			if err = qm.SuspendJob(jobID, nil, jerror); err != nil {
				logger.Error("(handleNoticeWorkDelivered:SuspendJob) jobID=%s; err=%s", jobID, errorMessage)
			}
			err = nil
		}

	}

	if errorCount > 0 {
		err = fmt.Errorf("(updateWorkflowInstancesMap) %d errors, last error message: %s", errorCount, lastError.Error())
		return
	}

	return
}

// ClientHandle _
func (qm *ServerMgr) ClientHandle() {
	logger.Info("(ServerMgr ClientHandle) starting")
	count := 0

	time.Sleep(3 * time.Second)

	for {
		//select {
		//case coReq := <-qm.coReq
		//logger.Debug(3, "(ServerMgr ClientHandle) try to pull work request")
		//coReq, err := qm.requestQueue.Pull()
		//for err != nil {
		//	time.Sleep(50 * time.Millisecond) // give clients time to put in requests or get a response
		//	time.Sleep(3 * time.Second)
		//	coReq, err = qm.requestQueue.Pull()
		//	logger.Debug(3, "(ServerMgr ClientHandle) waiting")
		//}
		//logger.Debug(3, "(ServerMgr ClientHandle) got work request")

		coReq := <-qm.coReq //written to in cqmgr.go
		count += 1
		request_start_time := time.Now()
		logger.Debug(3, "(ServerMgr ClientHandle) workunit checkout request received from client %s, Req=%v", coReq.fromclient, coReq)

		ok, err := qm.CQMgr.clientMap.Has(coReq.fromclient, true)
		if err != nil {
			logger.Warning("(ServerMgr ClientHandle) Could not get lock for client %s (%s)", coReq.fromclient, err.Error())
			continue
		}
		if !ok {
			logger.Error("(ServerMgr ClientHandle) Client %s not found. (It probably left in the mean-time)", coReq.fromclient)
			continue
		}

		var ack CoAck
		if qm.suspendQueue {
			// queue is suspended, return suspend error
			ack = CoAck{workunits: nil, err: errors.New(e.QueueSuspend)}
			logger.Debug(3, "(ServerMgr ClientHandle %s) nowworkunit: e.QueueSuspend", coReq.fromclient)
		} else {
			logger.Debug(3, "(ServerMgr ClientHandle %s) popWorks", coReq.fromclient)

			works, err := qm.popWorks(coReq)
			if err != nil {
				logger.Debug(3, "(ServerMgr ClientHandle) popWorks returned error: %s", err.Error())
			}
			logger.Debug(3, "(ServerMgr ClientHandle %s) popWorks done", coReq.fromclient)
			if err == nil {
				logger.Debug(3, "(ServerMgr ClientHandle %s) UpdateJobTaskToInProgress", coReq.fromclient)

				qm.UpdateJobTaskToInProgress(works)

				logger.Debug(3, "(ServerMgr ClientHandle %s) UpdateJobTaskToInProgress done", coReq.fromclient)
			}
			ack = CoAck{workunits: works, err: err}

			if len(works) > 0 {
				wu := works[0]

				logger.Debug(3, "(ServerMgr ClientHandle %s) workunit: %s", coReq.fromclient, wu.ID)
			} else {
				logger.Debug(3, "(ServerMgr ClientHandle %s) works is empty", coReq.fromclient)
			}
		}
		logger.Debug(3, "(ServerMgr ClientHandle %s) send response now", coReq.fromclient)

		start_time := time.Now()

		timer := time.NewTimer(20 * time.Second)

		select {
		case coReq.response <- ack:
			logger.Debug(3, "(ServerMgr ClientHandle %s) send response (maybe workunit) to client via response channel", coReq.fromclient)
		case <-timer.C:
			elapsed_time := time.Since(start_time)
			logger.Error("(ServerMgr ClientHandle %s) timed out after %s ", coReq.fromclient, elapsed_time)
			continue
		}
		logger.Debug(3, "(ServerMgr ClientHandle %s) done", coReq.fromclient)

		if count%100 == 0 { // use modulo to reduce number of log messages
			request_time_elapsed := time.Since(request_start_time)

			logger.Info("(ServerMgr ClientHandle) Responding to work request took %s", request_time_elapsed)
		}
	}
}

func (qm *ServerMgr) NoticeHandle() {
	logger.Info("(ServerMgr NoticeHandle) starting")
	for {
		notice := <-qm.feedback

		id, err := notice.ID.String()
		if err != nil {
			logger.Error("(NoticeHandle) notice.ID invalid: " + err.Error())
			err = nil
			continue
		}

		logger.Debug(3, "(ServerMgr NoticeHandle) got notice: workid=%s, status=%s, clientid=%s", id, notice.Status, notice.WorkerID)

		//fmt.Printf("Notice:")
		//spew.Dump(notice)
		err = qm.handleNoticeWorkDelivered(notice)
		if err != nil {

			logger.Error("(NoticeHandle) handleNoticeWorkDelivered returned: %s", err.Error())
			err = nil
			//fmt.Println(err.Error())
		}
	}
}

//--------queue status methods-------

func (qm *ServerMgr) SuspendQueue() {
	qm.suspendQueue = true
}

func (qm *ServerMgr) ResumeQueue() {
	qm.suspendQueue = false
}

func (qm *ServerMgr) QueueStatus() string {
	if qm.suspendQueue {
		return "suspended"
	} else {
		return "running"
	}
}

func (qm *ServerMgr) GetQueue(name string) interface{} {
	if name == "job" {
		suspended_jobs := qm.GetSuspendJobs()
		return jQueueShow{qm.actJobs, suspended_jobs}
	}
	if name == "task" {
		tasks, err := qm.TaskMap.GetTasks()
		if err != nil {
			return err
		}
		return tasks
	}
	if name == "workall" {
		workunits, err := qm.workQueue.all.GetWorkunits()
		if err != nil {
			return err
		}
		return workunits
	}
	if name == "workqueue" {
		workunits, err := qm.workQueue.Queue.GetWorkunits()
		if err != nil {
			return err
		}
		return workunits
	}
	if name == "workcheckout" {
		workunits, err := qm.workQueue.Checkout.GetWorkunits()
		if err != nil {
			return err
		}
		return workunits
	}
	if name == "worksuspend" {
		workunits, err := qm.workQueue.Suspend.GetWorkunits()
		if err != nil {
			return err
		}
		return workunits
	}
	if name == "client" {
		return &qm.clientMap
	}
	return nil
}

//--------suspend job accessor methods-------

func (qm *ServerMgr) lenSusJobs() (l int) {

	l = 0
	jobs, _ := JM.Get_List(true) // TODO error handling

	for i := range jobs {
		job := jobs[i]
		state, _ := job.GetState(true)
		if state == JOB_STAT_SUSPEND {
			l += 1
		}

	}

	//qm.sjLock.RLock()
	//l = len(qm.susJobs)
	//qm.sjLock.RUnlock()
	return
}

//func (qm *ServerMgr) putSusJob(id string) {
//	qm.sjLock.Lock()
//	qm.susJobs[id] = true
//	qm.sjLock.Unlock()
//}

func (qm *ServerMgr) GetSuspendJobs() (sjobs map[string]bool) {

	jobs, _ := JM.Get_List(true) // TODO error handling

	sjobs = make(map[string]bool)

	for i := range jobs {
		job := jobs[i]
		state, _ := job.GetState(true) // TODO error handling
		if state == JOB_STAT_SUSPEND {
			id, _ := job.GetID(true)
			sjobs[id] = true
		}

	}

	// qm.sjLock.RLock()
	// 	defer qm.sjLock.RUnlock()
	// 	sjobs = make(map[string]bool)
	// 	for id, _ := range qm.susJobs {
	// 		sjobs[id] = true
	// 	}
	return
}

//func (qm *ServerMgr) removeSusJob(id string) {
//	qm.sjLock.Lock()
//	delete(qm.susJobs, id)
//	qm.sjLock.Unlock()
//}

func (qm *ServerMgr) isSusJob(id string) (has bool) {

	job, err := GetJob(id)
	if err != nil {
		return
	}

	jobState, err := job.GetState(true)
	if err != nil {
		return
	}

	has = false
	if jobState == JOB_STAT_COMPLETED {
		has = true
	}
	return
	//	qm.sjLock.RLock()
	//	defer qm.sjLock.RUnlock()
	//	if _, ok := qm.susJobs[id]; ok {
	//		has = true
	//	} else {
	//		has = false
	//	}
	//	return
}

//--------active job accessor methods-------

func (qm *ServerMgr) copyJobPerf(a *JobPerf) (b *JobPerf) {
	b = new(JobPerf)
	*b = *a
	return
}

func (qm *ServerMgr) lenActJobs() (l int) {
	qm.ajLock.RLock()
	l = len(qm.actJobs)
	qm.ajLock.RUnlock()
	return
}

func (qm *ServerMgr) putActJob(jperf *JobPerf) {
	qm.ajLock.Lock()
	qm.actJobs[jperf.Id] = jperf
	qm.ajLock.Unlock()
}

func (qm *ServerMgr) getActJob(id string) (*JobPerf, bool) {
	qm.ajLock.RLock()
	defer qm.ajLock.RUnlock()
	if jobperf, ok := qm.actJobs[id]; ok {
		copy := qm.copyJobPerf(jobperf)
		return copy, true
	}
	return nil, false
}

func (qm *ServerMgr) GetActiveJobs() (ajobs map[string]bool) {
	qm.ajLock.RLock()
	defer qm.ajLock.RUnlock()
	ajobs = make(map[string]bool)
	for id, _ := range qm.actJobs {
		ajobs[id] = true
	}
	return
}

func (qm *ServerMgr) removeActJob(id string) {
	qm.ajLock.Lock()
	delete(qm.actJobs, id)
	qm.ajLock.Unlock()
}

func (qm *ServerMgr) isActJob(id string) (ok bool) {
	qm.ajLock.RLock()
	defer qm.ajLock.RUnlock()
	_, ok = qm.actJobs[id]
	return
}

//--------server methods-------

//updateQueue poll ready tasks and push into workQueue
func (qm *ServerMgr) updateQueue(logTimes bool) (err error) {

	logger.Debug(3, "(updateQueue) wait for lock")
	qm.queueLock.Lock()
	defer qm.queueLock.Unlock()

	logger.Debug(3, "(updateQueue) starting")
	var tasks []*Task
	tasks, err = qm.TaskMap.GetTasks()
	if err != nil {
		return
	}

	if true {
		if len(tasks) > 0 {
			task := tasks[0]
			jobID := task.JobId
			job, _ := GetJob(jobID)

			jobState, _ := job.GetState(true)
			fmt.Printf("*** job *** %s %s\n", jobID, jobState)
			for wi_id, _ := range job.WorkflowInstancesMap {
				wi := job.WorkflowInstancesMap[wi_id]
				wiState, _ := wi.GetState(true)
				fmt.Printf("WorkflowInstance: %s (%s) remain: %d\n", wi_id, wiState, wi.RemainSteps)

				var wi_tasks []*Task
				wi_tasks, err = wi.GetTasks(true)
				if err != nil {
					return
				}

				if len(wi_tasks) > 0 {
					for j, _ := range wi_tasks {
						task := wi_tasks[j]
						fmt.Printf("  Task %d: %s (wf: %s, state %s)\n", j, task.ID, task.WorkflowInstanceID, task.State)

					}
				} else {
					fmt.Printf("  no tasks\n")
				}
				//if len(wi_tasks) > 20 {
				//	panic("too many tasks!")
				//}

				for _, sw := range wi.Subworkflows {
					fmt.Printf("  Subworkflow: %s\n", sw)
				}

			}

		} else {
			fmt.Printf("*** job *** no tasks\n")
		}
	}
	//logger.Debug(0, "(updateQueue) len(tasks): %d", len(tasks))

	threads := 20
	size := len(tasks)

	loopStart := time.Now()
	logger.Debug(0, "(updateQueue) starting loop through TaskMap; threads: %d, TaskMap.Len: %d", threads, size)

	taskChan := make(chan *Task, size)
	queueChan := make(chan int, size)
	for w := 1; w <= threads; w++ {
		go qm.updateQueueWorker(w, logTimes, taskChan, queueChan)
	}

	//total := 0
	for _, task := range tasks {

		taskIDStr, _ := task.String()
		logger.Debug(3, "(updateQueue) sending task to QueueWorkers: %s", taskIDStr)

		//total += 1
		taskChan <- task
	}
	close(taskChan)

	// count tasks that have been queued (it also is a mean)
	notReady := 0
	queued := 0
	skipped := 0 // did not get lock
	for i := 1; i <= size; i++ {
		q := <-queueChan
		switch q {
		case 0:
			notReady++
		case 1:
			queued++
		case 2:
			skipped++
		}
	}
	close(queueChan)
	logger.Debug(0, "(updateQueue) completed loop through TaskMap; # processed: %d, queued: %d, skipped: %d, took %s", size, queued, skipped, time.Since(loopStart))

	logger.Debug(3, "(updateQueue) range qm.workQueue.Clean()")

	// Remove broken workunits
	for _, workunit := range qm.workQueue.Clean() {
		id := workunit.ID
		jobID := workunit.JobId
		taskID := workunit.TaskName

		jerror := &JobError{
			WorkFailed:  id,
			TaskFailed:  taskID,
			ServerNotes: "workunit is nil",
			Status:      JOB_STAT_SUSPEND,
		}
		if err = qm.SuspendJob(jobID, nil, jerror); err != nil {
			logger.Error("(updateQueue:SuspendJob) jobID=%s; err=%s", jobID, err.Error())
		}
		logger.Error("(updateQueue) workunit %s is nil, suspending job %s", id, jobID)
	}

	logger.Debug(3, "(updateQueue) ending")
	return
}

func (qm *ServerMgr) updateQueueWorker(id int, logTimes bool, taskChan <-chan *Task, queueChan chan<- int) {
	var taskSlow time.Duration
	taskSlow = 1 * time.Second
	for task := range taskChan {
		taskStart := time.Now()
		taskIDStr, _ := task.String()

		isQueued, times, skip, err := qm.updateQueueTask(task, logTimes)
		if err != nil {

			logger.Error("(updateQueueWorker) qm.updateQueueTask returned: %s", err.Error())

			jerror := &JobError{
				ClientFailed: "NA",
				WorkFailed:   "NA",
				TaskFailed:   taskIDStr,
				ServerNotes:  "updateQueueTask returned error: " + err.Error(),
				WorkNotes:    "NA",
				AppError:     "NA",
				Status:       JOB_STAT_SUSPEND,
			}
			err = nil

			_ = task.SetState(TASK_STAT_SUSPEND, true, "updateQueueWorker")

			jobID := task.JobId

			err = qm.SuspendJob(jobID, nil, jerror)
			if err != nil {
				logger.Error("(updateQueueWorker) SuspendJob failed: jobID=%s; err=%s", jobID, err.Error())
				err = nil
			}
		}
		if skip {
			queueChan <- 2 // skipped
			continue
		}
		if logTimes {
			taskTime := time.Since(taskStart)
			message := fmt.Sprintf("(updateQueueWorker) thread %d processed task: %s, took: %s, is queued %t", id, taskIDStr, taskTime, isQueued)
			if taskTime > taskSlow {
				message += fmt.Sprintf(", times: %+v", times)
			}
			logger.Info(message)
		}
		if isQueued {
			queueChan <- 1 // ok
		} else {
			queueChan <- 0 // not queued
		}
	}
}

// updateQueueTask
// returns skip if task is locked
func (qm *ServerMgr) updateQueueTask(task *Task, logTimes bool) (isQueued bool, times map[string]time.Duration, skip bool, err error) {
	skip = false
	var taskID Task_Unique_Identifier
	taskID, err = task.GetIDTimeout("updateQueueTask", time.Second*1)
	if err != nil {
		err = nil
		skip = true
		return
	}

	var taskIDStr string
	taskIDStr, _ = taskID.String()

	var taskState string
	taskState, err = task.GetStateTimeout(time.Second * 1)
	if err != nil {
		err = nil
		skip = true
		return
	}

	switch taskState {
	case TASK_STAT_READY:
		logger.Debug(3, "(updateQueueTask) task %s already has state %s, continue with enqueuing", taskIDStr, taskState)
	case TASK_STAT_INIT, TASK_STAT_PENDING:
		logger.Debug(3, "(updateQueueTask) task %s has state %s, first check if ready", taskIDStr, taskState)

		var taskReady bool
		var reason string
		startIsReady := time.Now()

		if logTimes {
			times = make(map[string]time.Duration)
		}
		taskReady, reason, skip, err = qm.isTaskReady(taskID, task)
		if logTimes {
			times["isTaskReady"] = time.Since(startIsReady)
		}
		if err != nil {
			err = fmt.Errorf("(updateQueueTask) qm.isTaskReady returned: %s", err.Error())
			return
		}
		if skip {
			return
		}
		if !taskReady {
			_ = task.SetTaskNotReadyReason(reason, true)

			logger.Debug(3, "(updateQueueTask) task not ready (%s): qm.isTaskReady returned reason: %s", taskIDStr, reason)
			return
		}
		logger.Debug(3, "(updateQueueTask) task ready (%s)", taskIDStr)

		// get new state
		taskState, err = task.GetState()
		if err != nil {
			return
		}

	default:
		logger.Debug(3, "(updateQueueTask) skipping task %s , it has state %s", taskIDStr, taskState)
		return
	}

	logger.Debug(3, "(updateQueueTask) task: %s (state: %s, added by %s)", taskIDStr, taskState, task.Comment)

	// task_ready
	if taskState != TASK_STAT_READY {
		err = fmt.Errorf("(updateQueueTask) task is not ready !???!?")
		return
	}
	logger.Debug(3, "(updateQueueTask) task %s is ready now, continue to enqueuing", taskIDStr)

	var jobID string
	jobID, err = task.GetJobID()
	if err != nil {
		return
	}

	var job *Job
	job, err = GetJob(jobID)
	if err != nil {
		return
	}

	startEnQueue := time.Now()
	var teqTimes map[string]time.Duration
	teqTimes, err = qm.taskEnQueue(taskID, task, job, logTimes)
	if logTimes {
		for k, v := range teqTimes {
			times[k] = v
		}
		times["taskEnQueue"] = time.Since(startEnQueue)
	}
	if err != nil {
		xerr := err
		err = nil

		logger.Error("(updateQueueTask) (taskIDStr: %s) suspending task, taskEnQueue returned: %s", taskIDStr, xerr.Error())
		err = task.SetState(TASK_STAT_SUSPEND, true, "updateQueueTask")
		if err != nil {
			return
		}

		jobID, err = task.GetJobID()
		if err != nil {
			return
		}

		var task_str string
		task_str, err = task.String()
		if err != nil {
			return

		}

		jerror := &JobError{
			TaskFailed:  task_str,
			ServerNotes: fmt.Sprintf("failed enqueuing task %s, qm.taskEnQueue returned: %s", taskIDStr, xerr.Error()),
			Status:      JOB_STAT_SUSPEND,
		}
		err = qm.SuspendJob(jobID, nil, jerror)
		if err != nil {
			err = fmt.Errorf("(updateQueueTask) qm.SuspendJob returned: %s", err.Error())
			return
		}
		err = xerr
		return
	}

	isQueued = true

	logger.Debug(3, "(updateQueueTask) task enqueued: %s", taskIDStr)

	return
}

func RemoveWorkFromClient(client *Client, workid Workunit_Unique_Identifier) (err error) {
	err = client.AssignedWork.Delete(workid, true)
	if err != nil {
		return
	}

	work_length, err := client.AssignedWork.Length(true)
	if err != nil {
		return
	}

	if work_length > 0 {

		clientid, _ := client.GetID(true)

		logger.Error("(RemoveWorkFromClient) Client %s still has %d workunits assigned, after delivering one workunit", clientid, work_length)

		assigned_workIDs, err := client.AssignedWork.Get_list(true)
		if err != nil {
			return err
		}
		for _, workID := range assigned_workIDs {
			_ = client.AssignedWork.Delete(workID, true)
		}

		work_length, err = client.AssignedWork.Length(true)
		if err != nil {
			return err
		}
		if work_length > 0 {
			logger.Error("(RemoveWorkFromClient) Client still has work assigned, even after everything should have been deleted.")
			return fmt.Errorf("(RemoveWorkFromClient) Client %s still has %d workunits", clientid, work_length)
		}
	}
	return
}

// invoked for every completed workunit
// updates task object:
//  - every workunit will decrease counter task.RemainWork
//  - last workunit will complete the task
// ** only last workunit writes CWL results into task ! **
// ** other workunits contribute only their rank information **
func (qm *ServerMgr) handleWorkStatDone(client *Client, clientid string, task *Task, workid Workunit_Unique_Identifier, notice *Notice) (err error) {
	//log event about work done (WD)

	computetime := notice.ComputeTime

	var workStr string
	workStr, err = workid.String()
	if err != nil {
		err = fmt.Errorf("(handleWorkStatDone) workid.String() returned: %s", err.Error())
		return
	}
	//workid_string := workid.String()

	logger.Event(event.WORK_DONE, "workid="+workStr+";clientid="+clientid)
	//update client status

	var task_str string
	task_str, err = task.String()
	if err != nil {
		return
	}

	defer func() {
		//done, remove from the workQueue
		qm.workQueue.Delete(workid)
	}()

	if client != nil {
		err = client.IncrementTotalCompleted()
		if err != nil {
			err = fmt.Errorf("(handleWorkStatDone) client.IncrementTotalCompleted returned: %s", err.Error())
			return
		}
	}
	var remain_work int
	remain_work, err = task.IncrementRemainWork(-1, true)
	if err != nil {
		err = fmt.Errorf("(handleWorkStatDone) client=%s work=%s task.IncrementRemainWork returned: %s", clientid, workStr, err.Error())
		return
	}

	// double check, remain_work should equal # of workunits in workqueue with same job and task ids
	workunits, werr := qm.workQueue.GetAll()
	if werr != nil {
		err = fmt.Errorf("(handleWorkStatDone) unable to get workunit list: %s", werr.Error())
		return
	}

	var workunit_count int
	for _, wu := range workunits {
		if (wu.JobId == workid.JobId) && (wu.TaskName == workid.TaskName) && (wu.Rank != workid.Rank) {
			workunit_count += 1
		}
	}
	if workunit_count != remain_work {
		err = fmt.Errorf("(handleWorkStatDone) client=%s work=%s remainwork (%d) does not match number of workunits in queue (%d)", clientid, workStr, remain_work, workunit_count)
		return
	}

	err = task.IncrementComputeTime(computetime)
	if err != nil {
		err = fmt.Errorf("(handleWorkStatDone) client=%s work=%s IncrementComputeTime returned: %s", clientid, workStr, err.Error())
		return
	}

	logger.Debug(3, "(handleWorkStatDone) remain_work: %d (%s)", remain_work, workStr)

	if remain_work == 0 {
		err = qm.handleLastWorkunit(clientid, task, task_str, workStr, notice)
		if err != nil {
			err = fmt.Errorf("(handleWorkStatDone) handleLastWorkunit returned: %s", err.Error())
			return
		}
	}

	return
}

// ****************************
// ******* LAST WORKUNIT ******
// ****************************
func (qm *ServerMgr) handleLastWorkunit(clientid string, task *Task, task_str string, workStr string, notice *Notice) (err error) {

	// validate file sizes of all outputs
	err = task.ValidateOutputs() // for AWE1 only
	if err != nil {
		// we create job error object and suspend job
		err_msg := fmt.Sprintf("(handleLastWorkunit) ValidateOutputs returned: %s", err.Error())
		jerror := &JobError{
			ClientFailed: clientid,
			WorkFailed:   workStr,
			TaskFailed:   task_str,
			ServerNotes:  err_msg,
			Status:       JOB_STAT_SUSPEND,
		}
		err = task.SetState(TASK_STAT_SUSPEND, true, "handleLastWorkunit")
		if err != nil {
			err = fmt.Errorf("(handleLastWorkunit) task.SetState returned: %s", err.Error())
			return
		}
		err = qm.SuspendJob(task.JobId, nil, jerror)
		if err != nil {
			err = fmt.Errorf("(handleLastWorkunit) SuspendJob returned: %s", err.Error())
			return
		}
		err = errors.New(err_msg)
		return
	}

	// **************************************
	// ******* write results into task ******
	// **************************************
	if task.WorkflowStep != nil {
		err = task.SetProcessOutput(*notice.Results, true)
		if err != nil {
			err = fmt.Errorf("(handleLastWorkunit) task.SetStepOutput returned: %s", err.Error())
			return
		}
	}

	//if task.WorkflowStep == nil {
	//	err = fmt.Errorf("(handleLastWorkunit) task.WorkflowStep == nil")
	//	return
	//}

	// iterate over expected outputs

	//var process interface{}
	//process_cached := false

	//	if task.WorkflowStep == nil {
	//		err = fmt.Errorf("(handleLastWorkunit) task.WorkflowStep == nil")
	//		return
	//	}

	//	if task.Scatter_parent == nil && false {
	//		for i, _ := range task.WorkflowStep.Out {
	//			step_output := &task.WorkflowStep.Out[i]
	//			basename := path.Base(step_output.Id)

	var wi *WorkflowInstance

	if task.WorkflowStep != nil {

		// var job *Job
		// job, err = task.GetJob(time.Second * 30)
		// if err != nil {
		// 	err = fmt.Errorf("(handleLastWorkunit) GetJob returned: %s", err.Error())
		// 	return
		// }

		//context := job.WorkflowContext

		//if task.Scatter_parent == nil {

		// map process output to step output
		stepOutputArray := &cwl.Job_document{}
		logger.Debug(3, "(handleLastWorkunit) len(task.WorkflowStep.Out): %d", len(task.WorkflowStep.Out))
		for i, _ := range task.WorkflowStep.Out {
			step_output := &task.WorkflowStep.Out[i]
			basename := path.Base(step_output.Id)

			//step_output_array := []cwl.NamedCWLType(*task.StepOutput)
			processOutputArray := []cwl.NamedCWLType(*task.ProcessOutput)

			// find in real outputs
			found := false
			for j, _ := range processOutputArray { // []cwl.NamedCWLType
				named := &processOutputArray[j]
				actual_output_base := path.Base(named.ID)
				if basename == actual_output_base {
					// add object to context using stepoutput name
					logger.Debug(3, "(handleLastWorkunit) adding %s ...", step_output.Id)

					stepOutputArray = stepOutputArray.Add(basename, named.Value)

					//err = context.Add(step_output.Id, named.Value, "handleLastWorkunit_1")
					//if err != nil {
					//	err = fmt.Errorf("(handleLastWorkunit) context.Add returned: %s", err.Error())
					//	return
					//}
					found = true
					continue
				}

			}
			if !found {
				var obj cwl.CWLObject
				obj = cwl.NewNull()
				// err = context.AddObject(step_output.Id, obj, "handleLastWorkunit_2") // TODO: DO NOT DO THIS FOR SCATTER TASKS
				// if err != nil {
				// 	err = fmt.Errorf("(handleLastWorkunit) context.Add returned: %s", err.Error())
				// 	return
				// }
				var objt cwl.CWLType
				objt = obj.(cwl.CWLType)
				stepOutputArray = stepOutputArray.Add(basename, objt)
				// check if this is an optional output in the tool

				//err = fmt.Errorf("(handleWorkStatDone) expected output not found: %s", basename)
				//return
			}
		}

		logger.Debug(3, "(handleLastWorkunit) call task.SetStepOutput with %d outputs", stepOutputArray.Len())
		err = task.SetStepOutput(*stepOutputArray, true)
		if err != nil {
			err = fmt.Errorf("(handleLastWorkunit) task.SetStepOutput returned: %s", err.Error())
			return
		}
		//}

		var ok bool
		wi, ok, err = task.GetWorkflowInstance(true)
		if err != nil {
			err = fmt.Errorf("(handleLastWorkunit) task.GetWorkflowInstance returned: %s", err.Error())
			return
		}

		if !ok {
			err = fmt.Errorf("(handleLastWorkunit) Did not get WorkflowInstance from task")
			return
		}
	}
	// err = task.SetState(wi, TASK_STAT_COMPLETED, true)
	// if err != nil {
	// 	err = fmt.Errorf("(handleWorkStatDone) task.SetState returned: %s", err.Error())
	// 	return
	// }

	//log event about task done (TD)
	err = qm.FinalizeTaskPerf(task)
	if err != nil {
		err = fmt.Errorf("(handleLastWorkunit) FinalizeTaskPerf returned: %s", err.Error())
		return
	}
	logger.Event(event.TASK_DONE, "taskID="+task_str)

	//update the info of the job which the task is belong to, could result in deletion of the
	//task in the task map when the task is the final task of the job to be done.
	err = qm.taskCompleted(wi, task) //task state QUEUED -> COMPLETED
	if err != nil {
		err = fmt.Errorf("(handleLastWorkunit) taskCompleted returned: %s", err.Error())
		return
	}
	return

}

// handle feedback from a client about the execution of a workunit
// successful workunits are handeled by "handleWorkStatDone()"
func (qm *ServerMgr) handleNoticeWorkDelivered(notice Notice) (err error) {

	logger.Debug(3, "(handleNoticeWorkDelivered) start")
	clientid := notice.WorkerID

	workID := notice.ID
	taskID := workID.GetTask()

	jobID := workID.JobId

	noticeStatus := notice.Status

	//computetime := notice.ComputeTime
	notes := notice.Notes

	var workStr string
	workStr, err = workID.String()
	if err != nil {
		err = fmt.Errorf("(handleNoticeWorkDelivered) workID.String() returned: %s", err.Error())
		return
	}

	logger.Debug(3, "(handleNoticeWorkDelivered) workid: %s status: %s client: %s", workStr, noticeStatus, clientid)

	// we should not get here, but if we do then return error
	if noticeStatus == WORK_STAT_DISCARDED {
		logger.Error("(handleNoticeWorkDelivered) [warning] skip status change: workid=%s status=%s", workStr, noticeStatus)
		return
	}

	var client *Client
	client = nil
	if clientid != "_internal" {
		// *** Get Client

		var ok bool
		client, ok, err = qm.GetClient(clientid, true)
		if err != nil {
			return
		}
		if !ok {
			err = fmt.Errorf("(handleNoticeWorkDelivered) client not found")
			return
		}
		defer RemoveWorkFromClient(client, workID)
	}
	// *** Get Task
	var task *Task
	var tok bool
	task, tok, err = qm.TaskMap.Get(taskID, true)
	if err != nil {
		return
	}
	if !tok {
		//task not existed, possible when job is deleted before the workunit done
		err = fmt.Errorf("(handleNoticeWorkDelivered) task %s for workunit %s not found", taskID, workStr)
		logger.Error(err.Error())
		qm.workQueue.Delete(workID)
		return
	}

	reason := ""

	// *** Get workunit
	var work *Workunit
	var wok bool
	work, wok, err = qm.workQueue.Get(workID)
	if err != nil {
		return
	}
	if !wok {
		err = fmt.Errorf("(handleNoticeWorkDelivered) workunit %s not found in workQueue", workStr)
		return
	}
	workState := work.State

	if workState != WORK_STAT_CHECKOUT && workState != WORK_STAT_RESERVED {
		err = fmt.Errorf("(handleNoticeWorkDelivered) workunit %s did not have state WORK_STAT_CHECKOUT or WORK_STAT_RESERVED (state is %s)", workStr, workState)
		return
	}

	if noticeStatus == WORK_STAT_SUSPEND {
		reason = "workunit suspended by worker" // TODO add more info from worker
	}

	// *** update state of workunit
	err = qm.workQueue.StatusChange(Workunit_Unique_Identifier{}, work, noticeStatus, reason)
	if err != nil {
		err = fmt.Errorf("(handleNoticeWorkDelivered) qm.workQueue.StatusChange returned: %s", err.Error())
		return
	}

	err = task.LockNamed("handleNoticeWorkDelivered/noretry")
	if err != nil {
		return
	}
	noretry := task.Info.NoRetry
	task.Unlock()

	var MAX_FAILURE int
	if noretry == true {
		MAX_FAILURE = 1
	} else {
		MAX_FAILURE = conf.MAX_WORK_FAILURE
	}

	var taskState string
	taskState, err = task.GetState()
	if err != nil {
		return
	}

	if taskState == TASK_STAT_FAIL_SKIP {
		// A work unit for this task failed before this one arrived.
		// User set Skip=2 so the task was just skipped. Any subsiquent
		// workunits are just deleted...
		_ = qm.workQueue.Delete(workID)
		err = fmt.Errorf("(handleNoticeWorkDelivered) workunit %s failed due to skip", workStr)
		return
	}

	logger.Debug(3, "(handleNoticeWorkDelivered) handling status %s", noticeStatus)
	switch noticeStatus {
	case WORK_STAT_DONE:
		//      ******************
		//      * WORK_STAT_DONE *
		//      ******************
		err = qm.handleWorkStatDone(client, clientid, task, workID, &notice)
		if err != nil {
			err = fmt.Errorf("(handleNoticeWorkDelivered) handleWorkStatDone returned: %s", err.Error())
			return
		}
	case WORK_STAT_FAILED_PERMANENT: // (special case !) failed and cannot be recovered

		logger.Event(event.WORK_FAILED, "workid="+workStr+";clientid="+clientid)
		logger.Debug(3, "(handleNoticeWorkDelivered) work failed permanently (status=%s) workid=%s clientid=%s", noticeStatus, workStr, clientid)
		work.Failed++

		//qm.workQueue.StatusChange(Workunit_Unique_Identifier{}, work, WORK_STAT_FAILED_PERMANENT, "")

		err = task.SetState(TASK_STAT_FAILED_PERMANENT, true, "handleNoticeWorkDelivered")
		if err != nil {
			return
		}

		var taskStr string
		taskStr, err = task.String()
		if err != nil {
			err = fmt.Errorf("(handleNoticeWorkDelivered) task.String returned: %s", err.Error())
			return
		}

		jerror := &JobError{
			ClientFailed: clientid,
			WorkFailed:   workStr,
			TaskFailed:   taskStr,
			ServerNotes:  "exit code 42 encountered",
			WorkNotes:    notes,
			AppError:     notice.Stderr,
			Status:       JOB_STAT_FAILED_PERMANENT,
		}
		err = qm.SuspendJob(jobID, nil, jerror)
		if err != nil {
			logger.Error("(handleNoticeWorkDelivered:SuspendJob) jobID=%s; err=%s", jobID, err.Error())
		}
	case WORK_STAT_ERROR: //workunit failed, requeue or put it to suspend list
		logger.Event(event.WORK_FAIL, "workid="+workStr+";clientid="+clientid)
		logger.Debug(3, "(handleNoticeWorkDelivered) work failed (status=%s, notes: %s) workid=%s clientid=%s", noticeStatus, notes, workStr, clientid)

		work.Failed++

		if work.Failed < MAX_FAILURE {
			qm.workQueue.StatusChange(Workunit_Unique_Identifier{}, work, WORK_STAT_QUEUED, "")
			logger.Event(event.WORK_REQUEUE, "workid="+workStr)
		} else {
			//failure time exceeds limit, suspend workunit, task, job
			err = qm.workQueue.StatusChange(Workunit_Unique_Identifier{}, work, WORK_STAT_SUSPEND, "work.Failed >= MAX_FAILURE")
			if err != nil {
				err = fmt.Errorf("(handleNoticeWorkDelivered) qm.workQueue.StatusChange returned: %s", err.Error())
				return
			}

			logger.Event(event.WORK_SUSPEND, "workid="+workStr)

			if err = task.SetState(TASK_STAT_SUSPEND, true, "handleNoticeWorkDelivered"); err != nil {
				err = fmt.Errorf("(handleNoticeWorkDelivered) task.SetState returned: %s", err.Error())
				return
			}

			var taskStr string
			taskStr, err = task.String()
			if err != nil {
				err = fmt.Errorf("(handleNoticeWorkDelivered) task.String returned: %s", err.Error())
				return
			}
			jerror := &JobError{
				ClientFailed: clientid,
				WorkFailed:   workStr,
				TaskFailed:   taskStr,
				ServerNotes:  fmt.Sprintf("workunit failed %d time(s)", MAX_FAILURE),
				WorkNotes:    notes,
				AppError:     notice.Stderr,
				Status:       JOB_STAT_SUSPEND,
			}

			if task.WorkflowInstanceID != "" {
				var workflowInstance *WorkflowInstance
				var ok bool
				workflowInstance, ok, err = task.GetWorkflowInstance(true)
				if err != nil {
					logger.Error("(handleNoticeWorkDelivered) task.GetWorkflowInstance returned %s", err.Error())
					err = nil
					ok = false
				}

				if err == nil && !ok {
					logger.Error("(handleNoticeWorkDelivered) task.GetWorkflowInstance did not find workflowInstance")
				}

				if ok {
					err = workflowInstance.SetState(WIStateSuspended, true, "handleNoticeWorkDelivered")
					if err != nil {
						logger.Error("(handleNoticeWorkDelivered) workflowInstance.SetState returned %s", err.Error())
						err = nil
					}
				}

			}

			err = qm.SuspendJob(jobID, nil, jerror)
			if err != nil {
				logger.Error("(handleNoticeWorkDelivered:SuspendJob) jobID=%s; err=%s", jobID, err.Error())
			}
		}

		// Suspend client if needed
		var client *Client
		var ok bool
		client, ok, err = qm.GetClient(clientid, true)
		if err != nil {
			return
		}
		if !ok {
			err = fmt.Errorf(e.ClientNotFound)
			return
		}

		err = client.AppendSkipwork(workID, true)
		if err != nil {
			return
		}
		err = client.IncrementTotalFailed(true)
		if err != nil {
			return
		}

		var lastFailed int
		lastFailed, err = client.IncrementLastFailed(true)
		if err != nil {
			return
		}
		if lastFailed >= conf.MAX_CLIENT_FAILURE {
			qm.SuspendClient(clientid, client, "MAX_CLIENT_FAILURE on client reached", true)
		}
	default:
		err = fmt.Errorf("No handler for workunit status '%s' implemented (allowd: %s, %s, %s)", noticeStatus, WORK_STAT_DONE, WORK_STAT_FAILED_PERMANENT, WORK_STAT_ERROR)
		return
	}
	return
}

// GetJSONStatus _
func (qm *ServerMgr) GetJSONStatus() (status map[string]map[string]int, err error) {
	queuingWork, err := qm.workQueue.Queue.Len()
	if err != nil {
		return
	}
	var outWork int
	outWork, err = qm.workQueue.Checkout.Len()
	if err != nil {
		err = fmt.Errorf("(GetJSONStatus) qm.workQueue.Checkout.Len returtned: %s", err.Error())
		return
	}
	var suspendWork int
	suspendWork, err = qm.workQueue.Suspend.Len()
	if err != nil {
		err = fmt.Errorf("(GetJSONStatus) qm.workQueue.Suspend.Len returtned: %s", err.Error())
		return
	}
	var totalActiveWork int
	totalActiveWork, err = qm.workQueue.Len()
	if err != nil {
		err = fmt.Errorf("(GetJSONStatus) qm.workQueue.Len returtned: %s", err.Error())
		return
	}

	// *** jobs ***
	jobs := make(map[string]int)

	var jobList []*Job
	jobList, err = JM.Get_List(true)
	if err != nil {
		err = fmt.Errorf("(GetJSONStatus) JM.Get_List returtned: %s", err.Error())
		return
	}
	jobs["total"] = len(jobList)

	for _, job := range jobList {

		jobState, terr := job.GetStateTimeout(true, time.Second*1)
		if terr != nil {
			jobState = "unknown"
		}
		jobs[jobState]++

	}

	// *** tasks ***

	tasks := make(map[string]int)

	taskList, err := qm.TaskMap.GetTasks()
	if err != nil {
		return
	}
	tasks["total"] = len(taskList)

	for _, task := range taskList {

		taskState, terr := task.GetStateTimeout(time.Second * 1)

		if terr != nil {
			taskState = "unknown"
		}

		tasks[taskState]++

		// total_task += 1

		// switch task.State {
		// case TASK_STAT_COMPLETED:
		// 	completed_task += 1
		// case TASK_STAT_PENDING:
		// 	pending_task += 1
		// case TASK_STAT_QUEUED:
		// 	queuing_task += 1
		// case TASK_STAT_INPROGRESS:
		// 	started_task += 1
		// case TASK_STAT_SUSPEND:
		// 	suspended_task += 1
		// case TASK_STAT_SKIPPED:
		// 	skipped_task += 1
		// case TASK_STAT_FAIL_SKIP:
		// 	fail_skip_task += 1
		// }
	}
	//total_task -= skipped_task // user doesn't see skipped tasks

	// totalClient := 0
	// busyClient := 0
	// idleClient := 0
	// suspendClient := 0

	clientStates := make(map[string]int)

	clientList, err := qm.clientMap.GetClients()
	if err != nil {
		return
	}

	totalClient := len(clientList)
	for _, client := range clientList {

		clientStates[client.Status]++

		// if ! client.Healty

		// if client.Suspended {
		// 	suspendClient++
		// }
		// if client.Busy {
		// 	busyClient++
		// } else {
		// 	idleClient++
		// }

	}
	clientStates["total"] = totalClient

	//jobs := map[string]int{
	//	"total":     total_job,
	//	"active":    active_jobs,
	//	"suspended": suspend_job,
	//}
	// tasks := map[string]int{
	// 	"total":       total_task,
	// 	"queuing":     queuing_task,
	// 	"in-progress": started_task,
	// 	"pending":     pending_task,
	// 	"completed":   completed_task,
	// 	"suspended":   suspended_task,
	// 	"failed":      fail_skip_task,
	// }
	workunits := map[string]int{
		"total":     totalActiveWork,
		"queuing":   queuingWork,
		"checkout":  outWork,
		"suspended": suspendWork,
	}
	// clients := map[string]int{
	// 	"total":     totalClient,
	// 	"busy":      busyClient,
	// 	"idle":      idleClient,
	// 	"suspended": suspendClient,
	// }
	status = map[string]map[string]int{
		"jobs":      jobs,
		"tasks":     tasks,
		"workunits": workunits,
		"clients":   clientStates,
	}
	return
}

// GetTextStatus _ TODO: this will not reflect all states. Get rid of this, difficult to maintain!
func (qm *ServerMgr) GetTextStatus() string {
	status, _ := qm.GetJSONStatus() // TODO handle error
	statMsg := "++++++++AWE server queue status++++++++\n" +
		fmt.Sprintf("total jobs ............... %d\n", status["jobs"]["total"]) +
		fmt.Sprintf("    active:           (%d)\n", status["jobs"]["active"]) +
		fmt.Sprintf("    suspended:        (%d)\n", status["jobs"]["suspended"]) +
		fmt.Sprintf("total tasks .............. %d\n", status["tasks"]["total"]) +
		fmt.Sprintf("    queuing:          (%d)\n", status["tasks"]["queuing"]) +
		fmt.Sprintf("    in-progress:      (%d)\n", status["tasks"]["in-progress"]) +
		fmt.Sprintf("    pending:          (%d)\n", status["tasks"]["pending"]) +
		fmt.Sprintf("    completed:        (%d)\n", status["tasks"]["completed"]) +
		fmt.Sprintf("    suspended:        (%d)\n", status["tasks"]["suspended"]) +
		fmt.Sprintf("    failed & skipped: (%d)\n", status["tasks"]["failed"]) +
		fmt.Sprintf("total workunits .......... %d\n", status["workunits"]["total"]) +
		fmt.Sprintf("    queuing:          (%d)\n", status["workunits"]["queuing"]) +
		fmt.Sprintf("    checkout:         (%d)\n", status["workunits"]["checkout"]) +
		fmt.Sprintf("    suspended:        (%d)\n", status["workunits"]["suspended"]) +
		fmt.Sprintf("total clients ............ %d\n", status["clients"]["total"]) +
		fmt.Sprintf("    busy:             (%d)\n", status["clients"]["busy"]) +
		fmt.Sprintf("    idle:             (%d)\n", status["clients"]["idle"]) +
		fmt.Sprintf("    suspend:          (%d)\n", status["clients"]["suspended"]) +
		fmt.Sprintf("---last update: %s\n\n", time.Now())
	return statMsg
}

//---end of mgr methods

//--workunit methds (servermgr implementation)
// FetchDataToken _
func (qm *ServerMgr) FetchDataToken(workID Workunit_Unique_Identifier, clientid string) (token string, err error) {

	//precheck if the client is registered
	client, ok, err := qm.GetClient(clientid, true)
	if err != nil {
		return
	}
	if !ok {
		return "", errors.New(e.ClientNotFound)
	}

	is_suspended, err := client.GetSuspended(true)
	if err != nil {
		return
	}

	if is_suspended {
		err = errors.New(e.ClientSuspended)
		return
	}

	jobid := workID.JobId

	job, err := GetJob(jobid)
	if err != nil {
		return
	}
	token = job.GetDataToken()
	if token == "" {
		var workStr string
		workStr, err = workID.String()
		if err != nil {
			err = fmt.Errorf("(FetchDataToken) workid.String() returned: %s", err.Error())
			return
		}
		err = errors.New("no data token set for workunit " + workStr)
		return
	}
	return
}

// func (qm *ServerMgr) FetchPrivateEnvs_deprecated(workid string, clientid string) (envs map[string]string, err error) {
// 	//precheck if the client is registered
// 	client, ok, err := qm.GetClient(clientid, true)
// 	if err != nil {
// 		return
// 	}
// 	if !ok {
// 		return nil, errors.New(e.ClientNotFound)
// 	}
// 	client_status, err := client.Get_Status(true)
// 	if err != nil {
// 		return
// 	}
// 	if client_status == CLIENT_STAT_SUSPEND {
// 		return nil, errors.New(e.ClientSuspended)
// 	}
// 	jobid, err := GetJobIdByWorkId(workid)
// 	if err != nil {
// 		return nil, err
// 	}
//
// 	job, err := GetJob(jobid)
// 	if err != nil {
// 		return nil, err
// 	}
//
// 	taskid, _ := GetTaskIdByWorkId(workid)
//
// 	idx := -1
// 	for i, t := range job.Tasks {
// 		if t.Id == taskid {
// 			idx = i
// 			break
// 		}
// 	}
// 	envs = job.Tasks[idx].Cmd.Environ.Private
// 	if envs == nil {
// 		return nil, errors.New("no private envs for workunit " + workid)
// 	}
// 	return envs, nil
// }

// SaveStdLog _
func (qm *ServerMgr) SaveStdLog(id Workunit_Unique_Identifier, logname string, tmppath string) (err error) {
	savedpath, err := getStdLogPathByWorkID(id, logname)
	if err != nil {
		return err
	}
	os.Rename(tmppath, savedpath)
	return
}

// GetReportMsg _
func (qm *ServerMgr) GetReportMsg(id Workunit_Unique_Identifier, logname string) (report string, err error) {
	logpath, err := getStdLogPathByWorkID(id, logname)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(logpath); err != nil {
		return "", errors.New("log type '" + logname + "' not found")
	}

	content, err := ioutil.ReadFile(logpath)
	if err != nil {
		return "", err
	}
	return string(content), err
}

func getStdLogPathByWorkID(id Workunit_Unique_Identifier, logname string) (savedpath string, err error) {
	jobid := id.JobId

	var logdir string
	logdir, err = getPathByJobID(jobid)
	if err != nil {
		return
	}
	//workid := id.String()
	var workStr string
	workStr, err = id.String()
	if err != nil {
		err = fmt.Errorf("(getStdLogPathByWorkId) id.String() returned: %s", err.Error())
		return
	}

	savedpath = fmt.Sprintf("%s/%s.%s", logdir, workStr, logname)
	return
}

// this is trigggered by user action, either job POST or job resume / recover / resubmit
// func (qm *ServerMgr) EnqueueWorkflowInstancesByJobId(jobid string) (err error) {

// 	logger.Debug(3, "(EnqueueWorkflowInstancesByJobId) starting")
// 	job, err := GetJob(jobid)
// 	if err != nil {
// 		err = fmt.Errorf("(EnqueueWorkflowInstancesByJobId) GetJob returned: %s", err.Error())
// 		return
// 	}

// }

// EnqueueWorkflowInstance _
func (qm *ServerMgr) EnqueueWorkflowInstance(wi *WorkflowInstance) (err error) {

	logger.Debug(3, "(EnqueueWorkflowInstance) starting")

	wiUniqueID, _ := wi.GetID(true)

	err = GlobalWorkflowInstanceMap.Add(wiUniqueID, wi)
	if err != nil {
		err = fmt.Errorf("(EnqueueWorkflowInstancesByJobId) GlobalWorkflowInstanceMap.Add returned: %s", err.Error())
		return
	}

	return
}

// EnqueueTasks _
func (qm *ServerMgr) EnqueueTasks(tasks []*Task) (err error) {
	//logger.Debug(3, "(EnqueueTasksByJobId) starting")
	//job, err := GetJob(jobid)
	//if err != nil {
	//	err = fmt.Errorf("(EnqueueTasksByJobId) GetJob failed: %s", err.Error())
	//	return
	//}

	//fmt.Println("(EnqueueTasksByJobId) job.WorkflowInstances[0]:")
	//spew.Dump(job.WorkflowInstances[0])
	//panic("done")

	//var tasks []*Task
	//tasks, err = job.GetTasks()
	//if err != nil {
	//	err = fmt.Errorf("(EnqueueTasksByJobId) job.GetTasks failed: %s", err.Error())
	//	return
	//}

	task_len := len(tasks)
	logger.Debug(3, "(EnqueueTasks) got %d tasks", task_len)

	// err = job.SetState(JOB_STAT_QUEUING, nil)
	// if err != nil {
	// 	err = fmt.Errorf("(qmgr.taskEnQueue) UpdateJobState: %s", err.Error())
	// 	return
	// }

	//qm.CreateJobPerf(jobid)

	for _, task := range tasks {
		var taskState string
		taskState, err = task.GetState()
		if err != nil {
			return
		}

		if taskState == TASK_STAT_INPROGRESS || taskState == TASK_STAT_QUEUED {
			err = task.SetState(TASK_STAT_READY, true, "EnqueueTasks")
			if err != nil {
				return
			}
		} else if taskState == TASK_STAT_SUSPEND {
			err = task.SetState(TASK_STAT_PENDING, true, "EnqueueTasks")
			if err != nil {
				return
			}
		}

		// add to qm.TaskMap
		// updateQueue() process will actually enqueue the task
		// TaskMap.Add - makes it a pending task if init, throws error if task already in map with different pointer
		err = qm.TaskMap.Add(task, "EnqueueTasks")
		if err != nil {
			err = fmt.Errorf("(EnqueueTasks) qm.TaskMap.Add() returns: %s", err.Error())
			return
		}
	}

	return
}

// EnqueueTasksByJobId this is trigggered by user action, either job POST or job resume / recover / resubmit
func (qm *ServerMgr) EnqueueTasksByJobId(jobid string, caller string) (err error) {
	logger.Debug(3, "(EnqueueTasksByJobId) starting")
	job, err := GetJob(jobid)
	if err != nil {
		err = fmt.Errorf("(EnqueueTasksByJobId) GetJob failed: %s", err.Error())
		return
	}

	//fmt.Println("(EnqueueTasksByJobId) job.WorkflowInstances[0]:")
	//spew.Dump(job.WorkflowInstances[0])
	//panic("done")

	var tasks []*Task
	tasks, err = job.GetTasks()
	if err != nil {
		err = fmt.Errorf("(EnqueueTasksByJobId) job.GetTasks failed: %s", err.Error())
		return
	}

	task_len := len(tasks)
	logger.Debug(3, "(EnqueueTasksByJobId) got %d tasks", task_len)

	err = job.SetState(JOB_STAT_QUEUING, nil)
	if err != nil {
		err = fmt.Errorf("(qmgr.taskEnQueue) UpdateJobState: %s", err.Error())
		return
	}

	qm.CreateJobPerf(jobid)

	for _, task := range tasks {
		var taskState string
		taskState, err = task.GetState()
		if err != nil {
			return
		}

		if taskState == TASK_STAT_INPROGRESS || taskState == TASK_STAT_QUEUED {
			err = task.SetState(TASK_STAT_READY, true, "EnqueueTasksByJobId")
			if err != nil {
				return
			}
		} else if taskState == TASK_STAT_SUSPEND {
			err = task.SetState(TASK_STAT_PENDING, true, "EnqueueTasksByJobId")
			if err != nil {
				return
			}
		}

		// add to qm.TaskMap
		// updateQueue() process will actually enqueue the task
		// TaskMap.Add - makes it a pending task if init, throws error if task already in map with different pointer
		err = qm.TaskMap.Add(task, "EnqueueTasksByJobId/"+caller)
		if err != nil {
			err = fmt.Errorf("(EnqueueTasksByJobId) qm.TaskMap.Add() returns: %s", err.Error())
			return
		}
	}

	var jobState string
	jobState, err = job.GetState(true)
	if err != nil {
		return
	}
	if jobState != JOB_STAT_INPROGRESS {
		err = job.SetState(JOB_STAT_QUEUED, []string{JOB_STAT_INIT, JOB_STAT_SUSPEND, JOB_STAT_QUEUING})
		if err != nil {
			return
		}
	}

	return
}

// used by isTaskReady and is_WI_Ready
// check all WorkflowStepInputs for Source fields and checks if they are available
func (qm *ServerMgr) areSourceGeneratorsReady(step *cwl.WorkflowStep, job *Job, workflowInstance *WorkflowInstance) (ready bool, reason string, generators []string, err error) {

	logger.Debug(3, "(areSourceGeneratorsReady) start %s (number on inputs %d)", step.ID, len(step.In))

	workflowInstanceName := workflowInstance.LocalID

	for _, wsi := range step.In { // WorkflowStepInput

		//input_optional := false
		//if wsi.Default != nil {
		//	input_optional = true
		//}
		logger.Debug(3, "(areSourceGeneratorsReady) step input %s", wsi.ID)
		if wsi.Source == nil {
			logger.Debug(3, "(areSourceGeneratorsReady) step input %s source empty", wsi.ID)
			continue
		}

		sourceIsArray := false
		sourceAsArray, sourceIsArray := wsi.Source.([]interface{})

		if sourceIsArray {
			logger.Debug(3, "(areSourceGeneratorsReady) step input %s source_is_array", wsi.ID)
			for _, src := range sourceAsArray { // usually only one
				var srcStr string
				var ok bool

				srcStr, ok = src.(string)
				if !ok {

					err = fmt.Errorf("src is not a string")
					ready = false
					return
				}

				if !strings.HasPrefix(srcStr, "#") {
					srcStr = path.Join(workflowInstanceName, srcStr)
				}
				// see comments below
				context := job.WorkflowContext
				_, ok, err = context.Get(srcStr, true)
				if err != nil {
					err = fmt.Errorf("(areSourceGeneratorsReady) context.Get returned: %s", err.Error())
					return
				}
				if ok {
					continue
				}

				generator := path.Dir(srcStr)
				//generatorBase := path.Base(generator)
				//if len(generatorBase) == 36 { // TODO uuid check
				//	generator = path.Dir(generator)
				//}
				ok, reason, err = qm.isSourceGeneratorReady(job, workflowInstance, generator, false, job.WorkflowContext)
				if err != nil {
					err = fmt.Errorf("(areSourceGeneratorsReady) (type array, src_str: %s) isSourceGeneratorReady returns: %s", srcStr, err.Error())
					return
				}
				if !ok {
					reason = fmt.Sprintf(" (sourceIsArray) Generator not ready (%s)", reason)
					return
				}
				generators = append(generators, generator)
				logger.Debug(3, "(areSourceGeneratorsReady) step input (in array) %s is ready (generator: %s)", srcStr, generator)
			}
		} else {
			// not source_is_array
			logger.Debug(3, "(areSourceGeneratorsReady) step input %s NOT source_is_array", wsi.ID)
			var srcStr string
			var ok bool

			srcStr, ok = wsi.Source.(string)
			if !ok {
				err = fmt.Errorf("(areSourceGeneratorsReady) Cannot parse WorkflowStep source: %s", spew.Sdump(wsi.Source))
				return
			}
			if !strings.HasPrefix(srcStr, "#") {
				srcStr = path.Join(workflowInstanceName, srcStr)
			}

			logger.Debug(3, "(areSourceGeneratorsReady) %s.Source: %s", wsi.ID, srcStr)

			//if source is (workflow) input, generator does not need to be ready
			// instead of testing for workflow input, we just test if input already exists
			//context := job.WorkflowContext
			// _, ok, err = context.Get(srcStr, true)
			// if err != nil {
			// 	err = fmt.Errorf("(areSourceGeneratorsReady) context.Get returned: %s", err.Error())
			// 	return
			// }
			// if ok {
			// 	logger.Debug(3, "(areSourceGeneratorsReady) source %s found!", srcStr)
			// 	continue
			// }

			generator := path.Dir(srcStr)
			//generatorBase := path.Base(generator)
			//if len(generatorBase) == 36 { // TODO uuid check
			//	generator = path.Dir(generator)
			//}

			generatorArray := strings.Split(generator, "/")

			logger.Debug(3, "(areSourceGeneratorsReady) step input %s using generator %s", wsi.ID, generator)

			if len(generatorArray) == 1 { // workflow input
				generators = append(generators, "self")
				continue

			}

			// if generator == "." {

			// 	for x := range context.All {
			// 		fmt.Printf("context.All: %s\n", x)
			// 	}

			// 	err = fmt.Errorf("(areSourceGeneratorsReady) src \"%s\" is a workflow input, but was not found ", srcStr)
			// 	return
			// }

			ready, reason, err = qm.isSourceGeneratorReady(job, workflowInstance, generator, false, job.WorkflowContext)
			if err != nil {
				err = fmt.Errorf("(areSourceGeneratorsReady) B (type non-array, src_str: %s) isSourceGeneratorReady returns: %s", srcStr, err.Error())
				return
			}

			if !ready {
				reason = fmt.Sprintf(" (NOT sourceIsArray) Generator not ready (%s)", reason)
				return
			}
			generators = append(generators, generator)
			logger.Debug(3, "(areSourceGeneratorsReady) step input %s is ready (generator: %s)", srcStr, generator)
		}

	}
	ready = true
	logger.Debug(3, "(areSourceGeneratorsReady) finished, step.ID: %s", step.ID)
	return
}

// check whether a pending task is ready to enqueue (dependent tasks are all done)
// task is not locked
func (qm *ServerMgr) isTaskReady(taskID Task_Unique_Identifier, task *Task) (ready bool, reason string, skip bool, err error) {
	ready = false
	skip = false

	reason = "all ok"
	logger.Debug(3, "(isTaskReady) starting")

	taskState, err := task.GetStateNamedTimeout("isTaskReady", time.Second*1)
	if err != nil {
		err = nil
		skip = true
		return
	}
	logger.Debug(3, "(isTaskReady) task state at start is %s", taskState)

	if taskState == TASK_STAT_READY {
		ready = true
		return
	}

	if taskState == TASK_STAT_INIT || taskState == TASK_STAT_PENDING {
		logger.Debug(3, "(isTaskReady) task state is init or pending, continue:  %s", taskState)
	} else {
		err = fmt.Errorf("(isTaskReady) task has state %s, it does not make sense to test if it is ready", taskState)
		return
	}

	//taskID, err := task.GetId("isTaskReady")
	//if err != nil {
	//	return
	//}

	taskIDStr, _ := taskID.String()

	logger.Debug(3, "(isTaskReady %s)", taskIDStr)

	//skip if the belonging job is suspended
	//jobid, err := task.GetJobId()
	//if err != nil {
	//	return
	//}

	job, err := task.GetJob(time.Second*1, true)
	if err != nil {
		skip = true
		err = nil
		return
	}
	jobState, err := job.GetState(true)
	if err != nil {
		return
	}
	if jobState == JOB_STAT_SUSPEND {
		reason = "job is suspend"
		return
	}

	if task.Info != nil {
		info := task.Info
		if !info.StartAt.IsZero() {

			if info.StartAt.After(time.Now()) {
				// too early
				logger.Debug(3, "(isTaskReady %s) too early to execute (now: %s, StartAt: %s)", taskIDStr, time.Now(), info.StartAt)
				return
			} else {
				logger.Debug(3, "(isTaskReady %s) StartAt field is in the past, can execute now (now: %s, StartAt: %s)", taskIDStr, time.Now(), info.StartAt)
			}
		}
	}

	if task.WorkflowStep != nil {
		// check if CWL-style predecessors are all TASK_STAT_COMPLETED
		// ****** get inputs
		if job == nil {
			err = fmt.Errorf("(isTaskReady) job == nil")
			return
		}

		if job.WorkflowContext == nil {
			err = fmt.Errorf("(isTaskReady) job.WorkflowContext == nil")
			return
		}

		//job_input_map := *job.WorkflowContext.Job_input_map
		//if job_input_map == nil {
		//	err = fmt.Errorf("(isTaskReady) job.CWL_collection.Job_input_map is empty")
		//	return
		//}

		var workflowInstance *WorkflowInstance

		var ok bool
		workflowInstance, ok, err = job.GetWorkflowInstance(task.WorkflowInstanceID, true)
		if err != nil {
			err = fmt.Errorf("(isTaskReady) GetWorkflowInstance returned %s", err.Error())
			return
		}

		if !ok {
			//spew.Dump(job.WorkflowInstancesMap)

			for key, _ := range job.WorkflowInstancesMap {
				fmt.Printf("WorkflowInstancesMap: %s\n", key)
			}

			ready = false
			reason = fmt.Sprintf("(isTaskReady) WorkflowInstance not found: %s", task.WorkflowInstanceID)
			return
		}

		//workflowInputMap := workflowInstance.Inputs.GetMap()
		//workflowInstanceID := workflowInstance.LocalID
		//workflow_def := workflowInstance.Workflow_Definition

		//fmt.Println("WorkflowStep.Id: " + task.WorkflowStep.Id)
		var generators []string
		ready, reason, generators, err = qm.areSourceGeneratorsReady(task.WorkflowStep, job, workflowInstance)
		if err != nil {
			err = fmt.Errorf("(isTaskReady) areSourceGeneratorsReady returned: %s", err.Error())
			return
		}
		if !ready {
			reason = fmt.Sprintf("(isTaskReady) areSourceGeneratorsReady returned: %s", reason)
			return
		}
		logger.Debug(3, "(isTaskReady) areSourceGeneratorsReady reports task %s as ready (generators: %s)", taskIDStr, strings.Join(generators, ","))
	}

	if task.WorkflowStep == nil {
		// task read lock, check DependsOn list and IO.Origin list
		reason, skip, err = task.ValidateDependants(qm)
		if err != nil {
			err = fmt.Errorf("(isTaskReady) %s", err.Error())
			return
		}
		if skip {
			return
		}
		if reason != "" {
			reason = "ValidateDependants returned: " + reason
			return
		}
	}

	// now we are ready
	err = task.SetState(TASK_STAT_READY, true, "isTaskReady")
	if err != nil {
		err = fmt.Errorf("(isTaskReady) task.SetState returned: %s", err.Error())
		return
	}
	ready = true

	logger.Debug(3, "(isTaskReady) finished, task %s is ready", taskIDStr)
	return
}

// func (qm *ServerMgr) taskEnQueueWorkflow_deprecated(task *Task, job *Job, workflowInputMap cwl.JobDocMap, workflow *cwl.Workflow, logTimes bool) (times map[string]time.Duration, err error) {

// 	if workflow == nil {
// 		err = fmt.Errorf("(taskEnQueueWorkflow) workflow == nil !?")
// 		return
// 	}

// 	if len(task.ScatterChildren) > 0 {
// 		return
// 	}

// 	cwl_step := task.WorkflowStep
// 	taskID := task.Task_Unique_Identifier
// 	taskIDStr, _ := taskID.String()

// 	workflow_defintion_id := workflow.ID

// 	var workflowInstanceID string

// 	workflowInstanceID = taskID.TaskName

// 	parent_workflowInstanceID := task.WorkflowInstanceID

// 	// find inputs
// 	var task_input_array cwl.Job_document
// 	var task_input_map cwl.JobDocMap

// 	context := job.WorkflowContext

// 	if task.StepInput == nil {

// 		var workflowInstance *WorkflowInstance
// 		var ok bool
// 		workflowInstance, ok, err = job.GetWorkflowInstance(parent_workflowInstanceID, true)
// 		if err != nil {
// 			err = fmt.Errorf("(taskEnQueueWorkflow_deprecated) job.GetWorkflowInstance returned: %s", err.Error())
// 			return
// 		}
// 		if !ok {
// 			err = fmt.Errorf("(taskEnQueueWorkflow_deprecated) workflowInstance not found")
// 			return
// 		}

// 		var reason string
// 		task_input_map, ok, reason, err = qm.GetStepInputObjects(job, workflowInstance, workflowInputMap, cwl_step, context, "taskEnQueueWorkflow") // returns map[string]CWLType
// 		if err != nil {
// 			err = fmt.Errorf("(taskEnQueueWorkflow_deprecated) GetStepInputObjects returned: %s", err.Error())
// 			return
// 		}

// 		if !ok {
// 			err = fmt.Errorf("(taskEnQueueWorkflow_deprecated) GetStepInputObjects not ready, reason: %s", reason)
// 			return
// 		}

// 		if len(task_input_map) == 0 {
// 			err = fmt.Errorf("(taskEnQueueWorkflow_deprecated) A) len(task_input_map) == 0 (%s)", taskIDStr)
// 			return
// 		}

// 		task_input_array, err = task_input_map.GetArray()
// 		if err != nil {
// 			err = fmt.Errorf("(taskEnQueueWorkflow_deprecated) task_input_map.GetArray returned: %s", err.Error())
// 			return
// 		}
// 		task.StepInput = &task_input_array
// 		//task_input_map = task_input_array.GetMap()

// 	} else {
// 		task_input_array = *task.StepInput
// 		task_input_map = task_input_array.GetMap()
// 		if len(task_input_map) == 0 {
// 			err = fmt.Errorf("(taskEnQueueWorkflow_deprecated) B) len(task_input_map) == 0 ")
// 			return
// 		}
// 	}

// 	if strings.HasSuffix(task.TaskName, "/") {
// 		err = fmt.Errorf("(taskEnQueueWorkflow_deprecated) Slash at the end of TaskName!? %s", task.TaskName)
// 		return
// 	}

// 	// embedded workflows have a uniqe name relative to the parent workflow: e.g #entrypoint/steo0/<uuid>
// 	// stand-alone workflows have no unique name, e.g: #sometool

// 	//new_sub_workflow := ""
// 	//fmt.Printf("(taskEnQueueWorkflow) new_sub_workflow: %s - %s\n", task.Parent, task.TaskName)
// 	//if len(task.Parent) > 0 {
// 	//	new_sub_workflow = task.Parent + task.TaskName // TaskName starts with #, so we can split later
// 	//} else {
// 	//	new_sub_workflow = task.TaskName
// 	//}

// 	//new_sub_workflow := workflow_id

// 	//fmt.Printf("New Subworkflow: %s %s\n", task.Parent, task.TaskName)

// 	// New WorkflowInstance defined input nd ouput of this subworkflow
// 	// create tasks
// 	//var sub_workflow_tasks []*Task
// 	//sub_workflow_tasks, err = CreateWorkflowTasks(job, workflowInstanceID, workflow.Steps, workflow.Id, &taskID)
// 	//if err != nil {
// 	//	err = fmt.Errorf("(taskEnQueueWorkflow) CreateWorkflowTasks returned: %s", err.Error())
// 	//		return
// 	//	}

// 	var wi *WorkflowInstance

// 	wi, err = NewWorkflowInstance(workflowInstanceID, job.ID, workflow_defintion_id, job, parent_workflowInstanceID)
// 	if err != nil {
// 		err = fmt.Errorf("(taskEnQueueWorkflow_deprecated) NewWorkflowInstance returned: %s", err.Error())
// 		return
// 	}
// 	wi.Inputs = task_input_array
// 	err = wi.SetState(WIStatePending, false, "taskEnQueueWorkflow_deprecated")
// 	if err != nil {
// 		err = fmt.Errorf("(taskEnQueueWorkflow_deprecated) wi.SetState(WIStatePending returned: %s", err.Error())
// 		return
// 	}

// 	err = job.AddWorkflowInstance(wi, DbSyncTrue, true) // taskEnQueueWorkflow_deprecated
// 	if err != nil {
// 		err = fmt.Errorf("(taskEnQueueWorkflow_deprecated) job.AddWorkflowInstance returned: %s", err.Error())
// 		return
// 	}

// 	times = make(map[string]time.Duration)

// 	return
// }

//processInstanceEnQueueScatter _
// creates and enqueues scatter children
func (qm *ServerMgr) processInstanceEnQueueScatter(parentWorkflowInstance *WorkflowInstance, processInstance ProcessInstance, job *Job, workflowInputMap cwl.JobDocMap) (notice *Notice, err error) {
	notice = nil
	fmt.Printf("(processInstanceEnQueueScatter) start")

	// processInstance is either one:
	var task *Task
	var workflowInstance *WorkflowInstance

	if parentWorkflowInstance.Inputs == nil {
		err = fmt.Errorf("(processInstanceEnQueueScatter) parentWorkflowInstance.Inputs == nil")
		return
	}

	switch processInstance.(type) {
	case *Task:
		task = processInstance.(*Task)
	case *WorkflowInstance:
		workflowInstance = processInstance.(*WorkflowInstance)
	default:
		err = fmt.Errorf("(processInstanceEnQueueScatter) type not suppported")
		return
	}

	_ = workflowInstance
	// TODO store info that this has been evaluated
	//cwlStep := task.WorkflowStep

	// if processInstance.WorkflowStep == nil {
	// 	err = fmt.Errorf("(processInstanceEnQueueScatter) cwlStep == nil")
	// 	return
	// }

	var cwlStep *cwl.WorkflowStep
	cwlStep, err = processInstance.GetWorkflowStep(job)
	if err != nil {
		err = fmt.Errorf("(processInstanceEnQueueScatter) processInstance.GetWorkflowStep returned: %s", err.Error())
		return
	}

	if cwlStep == nil {
		err = fmt.Errorf("(processInstanceEnQueueScatter) cwlStep == nil")
		return
	}

	//taskID := task.Task_Unique_Identifier
	parentWiUUID := parentWorkflowInstance.ID

	if cwlStep.Scatter == nil {
		err = fmt.Errorf("(processInstanceEnQueueScatter) cwlStep.Scatter == nil")
		return
	}
	countOfScatterArrays := len(cwlStep.Scatter)

	scatterNamesMap := make(map[string]int, countOfScatterArrays)

	for i, name := range cwlStep.Scatter {
		nameBase := path.Base(name)
		//fmt.Printf("scatter_names_map, name_base: %s\n", name_base)
		scatterNamesMap[nameBase] = i
	}

	//  copy

	scatterMethod := cwlStep.ScatterMethod
	_ = scatterMethod

	scatterPositions := make([]int, countOfScatterArrays)
	scatterSourceStrings := make([]string, countOfScatterArrays) // an array of strings, where each string is a source pointing to an array

	nameToPostiton := make(map[string]int, countOfScatterArrays)

	scatterInputArrays := make([]cwl.Array, countOfScatterArrays)

	// search for scatter source arrays
	// fill map name_to_postiton and arrays scatter_positions and scatter_source_strings
	for i, scatterInputName := range cwlStep.Scatter {

		scatterInputNameBase := path.Base(scatterInputName)
		fmt.Printf("(processInstanceEnQueueScatter) scatterInput detected: %s\n", scatterInputName)

		nameToPostiton[scatterInputNameBase] = i // this just an inverse which is needed later

		scatterInputSourceStr := ""
		inputPosition := -1

		// search for workflow_step_input
		for j := range cwlStep.In {
			workflowStepInput := cwlStep.In[j]

			if path.Base(workflowStepInput.ID) == scatterInputNameBase {
				inputPosition = j
				break
			}
		}

		// error if workflow_step_input not found
		if inputPosition == -1 {
			// improve error message
			listOfInputs := ""
			for j := range cwlStep.In {
				workflowStepInput := cwlStep.In[j]
				listOfInputs += "," + path.Base(workflowStepInput.ID)
			}

			err = fmt.Errorf("(processInstanceEnQueueScatter) Input %s not found in list of step.Inputs (list: %s)", scatterInputNameBase, listOfInputs)
			return
		}

		workflowStepInput := cwlStep.In[inputPosition]
		scatterInputSource := workflowStepInput.Source

		switch scatterInputSource.(type) {
		case string:

			scatterInputSourceStr = scatterInputSource.(string)
		case []string, []interface{}:

			var scatterInputSourceArray []string

			scatterInputSourceArrayIf, ok := scatterInputSource.([]interface{})
			if ok {
				scatterInputSourceArray = []string{}
				for k := range scatterInputSourceArrayIf {
					var srcStr string
					srcStr, ok = scatterInputSourceArrayIf[k].(string)
					if !ok {
						err = fmt.Errorf("(processInstanceEnQueueScatter) element in source array is not a string")
						return
					}
					scatterInputSourceArray = append(scatterInputSourceArray, srcStr)
				}

			} else {

				scatterInputSourceArray = scatterInputSource.([]string)
			}

			scatterInputArrays[i], ok, err = qm.getCWLSourceArray(parentWorkflowInstance, workflowInputMap, job, scatterInputSourceArray, true)
			if err != nil {
				err = fmt.Errorf("(processInstanceEnQueueScatter) getCWLSourceArray returned: %s", err.Error())
				return
			}
			if !ok {
				err = fmt.Errorf("(processInstanceEnQueueScatter) element not found") // should not happen, error would have been thrown
				return
			}
			scatterInputSourceStr = "_array_"
		default:
			err = fmt.Errorf("(processInstanceEnQueueScatter) scatterInput_source is not string (%s)", reflect.TypeOf(scatterInputSource))
			return
		}

		scatterPositions[i] = inputPosition
		scatterSourceStrings[i] = scatterInputSourceStr

	}

	// get each scatter array and cast into array

	emptyArray := false // just for compliance tests
	for i := 0; i < countOfScatterArrays; i++ {

		scatterInput := cwlStep.Scatter[i]
		scatterInputSourceStr := scatterSourceStrings[i]
		fmt.Printf("scatterInputSourceStr: %s\n", scatterInputSourceStr)
		if scatterInputSourceStr == "_array_" {
			if scatterInputArrays[i].Len() == 0 {
				emptyArray = true
			}

			continue
		}
		// get array (have to cast into array still)
		var scatterInputObject cwl.CWLObject
		var ok bool
		scatterInputObject, ok, _, err = qm.getCWLSource(job, parentWorkflowInstance, workflowInputMap, scatterInputSourceStr, true, job.WorkflowContext)
		if err != nil {
			err = fmt.Errorf("(processInstanceEnQueueScatter) getCWLSource returned: %s", err.Error())
			return
		}
		if !ok {
			err = fmt.Errorf("(processInstanceEnQueueScatter) scatterInput %s not found", scatterInput)
			return
		}

		var scatterInputArrayPtr *cwl.Array
		scatterInputArrayPtr, ok = scatterInputObject.(*cwl.Array)
		if !ok {

			fmt.Println("parentWorkflowInstance:")
			spew.Dump(parentWorkflowInstance)

			//panic("sad")

			err = fmt.Errorf("(processInstanceEnQueueScatter) scatterInputObject type is not *cwl.Array, got %s ", reflect.TypeOf(scatterInputObject))
			return
		}
		//fmt.Println("parentWorkflowInstance:")
		//spew.Dump(parentWorkflowInstance)
		//panic("I am confused")

		if scatterInputArrayPtr.Len() == 0 {

			emptyArray = true
		}

		scatterInputArrays[i] = *scatterInputArrayPtr

	}
	scatterType := ""

	processStr := processInstance.GetIDStr()

	// dotproduct with 2 or more arrays
	if scatterMethod == "" || strings.ToLower(scatterMethod) == "dotproduct" {
		// requires that all arrays are the same length

		scatterType = "dot"

	} else if strings.ToLower(scatterMethod) == "nested_crossproduct" || strings.ToLower(scatterMethod) == "flat_crossproduct" {
		// arrays do not have to be the same length
		// nested_crossproduct and flat_crossproduct differ only in how results are merged

		if strings.ToLower(scatterMethod) == "nested_crossproduct" {
			// if task != nil {
			// 	task.SetState(TASK_STAT_SUSPEND, true)
			// } else {
			// 	workflowInstance.SetState(WIStateSuspended, true)
			// }
			processInstance.SetState(ProcessStatSuspend, true, "processInstanceEnQueueScatter")

			//taskStr, _ := task.String()

			jobID, _ := job.GetID(true)

			jerror := &JobError{
				TaskFailed:  "process failed: " + processStr,
				ServerNotes: fmt.Sprintf("nested_crossproduct not supported yet"),
				Status:      JOB_STAT_SUSPEND,
			}
			err = qm.SuspendJob(jobID, job, jerror)
			if err != nil {
				err = fmt.Errorf("(processInstanceEnQueueScatter) qm.SuspendJob returned: %s", err.Error())
				return
			}
		}

		// defined counter for iteration over all combinations
		scatterType = "cross"
	} else {
		err = fmt.Errorf("(processInstanceEnQueueScatter) Scatter type %s unknown", scatterMethod)
		return
	}
	// 1. Create template step with scatter inputs removed
	//cwl_step := task.WorkflowStep

	templateTaskStep := *cwlStep // this should make a copy , not nested copy
	templateTaskStep.Scatter = nil
	//fmt.Println("templateTaskStep inital:\n")
	//spew.Dump(templateTaskStep)

	// remove scatter
	var templateStepIn []cwl.WorkflowStepInput
	templateScatterStepIns := make(map[string]cwl.WorkflowStepInput, countOfScatterArrays)
	for i := range cwlStep.In {

		iInput := cwlStep.In[i]

		iInputIDBase := path.Base(iInput.ID)
		//fmt.Printf("iInputIDBase: %s\n", iInputIDBase)
		_, ok := scatterNamesMap[iInputIDBase] // skip scatter inputs
		if ok {
			templateScatterStepIns[iInputIDBase] = iInput // save scatter inputs in templateScatterStepIns
			continue
		}
		templateStepIn = append([]cwl.WorkflowStepInput{iInput}, templateStepIn...) // preprend iInput

	}

	//fmt.Println("templateScatterStepIns:")
	//spew.Dump(templateScatterStepIns)
	if len(templateScatterStepIns) == 0 {
		err = fmt.Errorf("(processInstanceEnQueueScatter) no scatter tasks found")
		return
	}

	// overwrite array, keep only non-scatter
	templateTaskStep.In = templateStepIn
	counter := NewSetCounter(countOfScatterArrays, scatterInputArrays, scatterType)

	if emptyArray {

		//task_already_finished = true

		var parentTask *Task
		var dummyTask *Task

		if task == nil {

			var workflowInstanceID string
			workflowInstanceID, err = workflowInstance.GetID(true)
			if err != nil {
				err = fmt.Errorf("(processInstanceEnQueueScatter) workflowInstance.GetID returned: %s", err.Error())
				return
			}
			var workflowInstanceUUID string
			workflowInstanceUUID, err = workflowInstance.GetUUID(true)
			if err != nil {
				err = fmt.Errorf("(processInstanceEnQueueScatter) workflowInstance.GetUUID returned: %s", err.Error())
				return
			}

			dummyTask, err = NewTask(job, workflowInstanceUUID, workflowInstanceID, "dummyTask")
			if err != nil {
				err = fmt.Errorf("(processInstanceEnQueueScatter) NewTask returned: %s", err.Error())
				return
			}
			parentTask = dummyTask

			err = workflowInstance.AddTask(job, dummyTask, true, true)
			if err != nil {
				err = fmt.Errorf("(processInstanceEnQueueScatter) workflowInstance.AddTask returned: %s", err.Error())
				return
			}
			err = qm.TaskMap.Add(dummyTask, "processInstanceEnQueueScatter")
			if err != nil {
				err = fmt.Errorf("(processInstanceEnQueueScatter) workflowInstance.AddTask returned: %s", err.Error())
				return
			}

		} else {
			parentTask = task
		}

		// create dummy Workunit
		var workunit *Workunit
		workunit, err = NewWorkunit(qm, parentTask, 0, job)
		if err != nil {
			err = fmt.Errorf("(processInstanceEnQueueScatter) Creation of fake workunit failed: %s", err.Error())
			return
		}
		qm.workQueue.Add(workunit)
		err = workunit.SetState(WORK_STAT_CHECKOUT, "internal processing")
		if err != nil {
			err = fmt.Errorf("(processInstanceEnQueueScatter) workunit.SetState returned: %s", err.Error())
			return
		}

		if dummyTask != nil {
			err = dummyTask.SetState(TASK_STAT_INPROGRESS, true, "processInstanceEnQueueScatter")
			if err != nil {
				err = fmt.Errorf("(processInstanceEnQueueScatter) dummyTask.SetState returned: %s", err.Error())
				return
			}
		}
		// create empty arrays for output and return notice

		notice = &Notice{}
		notice.WorkerID = "_internal"
		notice.ID = New_Workunit_Unique_Identifier(task.Task_Unique_Identifier, 0)
		notice.Status = WORK_STAT_DONE
		notice.ComputeTime = 0

		notice.Results = &cwl.Job_document{}

		for _, out := range cwlStep.Out {
			//
			outName := out.Id
			//fmt.Printf("outname: %s\n", out_name)

			newArray := &cwl.Array{}

			//new_out := cwl.NewNamedCWLType(out_name, new_array)
			//spew.Dump(*notice.Results)
			notice.Results = notice.Results.Add(outName, newArray)
			//spew.Dump(*notice.Results)
		}

		//fmt.Printf("len(notice.Results): %d\n", len(*notice.Results))

		return
	}

	//fmt.Println("templateTaskStep without scatter:")
	//spew.Dump(templateTaskStep)

	//if count_of_scatter_arrays == 1 {

	// create tasks
	var children []string
	//var newScatterTasks []*Task
	var newScatterProcesses []ProcessInstance

	counterRunning := true
	//processStr
	basename := path.Base(processStr)

	parentIDStr := strings.TrimSuffix(processStr, "/"+basename)

	if parentIDStr == processStr {
		err = fmt.Errorf("(processInstanceEnQueueScatter) parentIDStr == taskID.TaskName")
		return
	}

	for counterRunning {

		permutationInstance := ""
		for i := 0; i < counter.NumberOfSets-1; i++ {
			permutationInstance += strconv.Itoa(counter.Counter[i]) + "_"
		}
		permutationInstance += strconv.Itoa(counter.Counter[counter.NumberOfSets-1])

		scatterProcessName := basename + "_scatter" + permutationInstance

		// create sub process (task or workflowInstance)

		var subProcess ProcessInstance
		var subTask *Task
		var subWorkflowInstance *WorkflowInstance

		if task != nil {
			logger.Debug(3, "(processInstanceEnQueueScatter) New Task: parent: %s and scatterTaskName: %s", parentIDStr, scatterProcessName)

			subTask, err = NewTask(job, parentWiUUID, parentIDStr, scatterProcessName)
			if err != nil {
				err = fmt.Errorf("(processInstanceEnQueueScatter) (parentIDStr=%s) NewTask returned: %s", parentIDStr, err.Error())
				return
			}

			subProcess = subTask

			_, err = subTask.Init(job, job.ID)
			if err != nil {
				err = fmt.Errorf("(processInstanceEnQueueScatter) aweTask.Init() returned: %s", err.Error())
				return
			}

		} else {
			logger.Debug(3, "(processInstanceEnQueueScatter) New WorkflowInstance, parent: %s and scatterTaskName: %s", parentIDStr, scatterProcessName)
			scatterProcessNameComplete := path.Join(parentIDStr, scatterProcessName)
			subWorkflowInstance, err = NewWorkflowInstance(scatterProcessNameComplete, job.ID, parentWorkflowInstance.WorkflowDefinition, job, parentWiUUID)
			if err != nil {
				err = fmt.Errorf("(processInstanceEnQueueScatter) NewWorkflowInstance returned: %s", err.Error())
				return
			}
			subProcess = subWorkflowInstance

		}

		if subTask != nil {
			subTask.ScatterParent = &task.Task_Unique_Identifier

		} else {
			subWorkflowInstance.ScatterParent = workflowInstance.ID

		}
		//aweTask.Scatter_task = true

		// err = subProcess.SetState(ProcessStatInit, false, "processInstanceEnQueueScatter") // no need to lock yet
		// if err != nil {
		// 	err = fmt.Errorf("(processInstanceEnQueueScatter) subProcess.SetState returned: %s", err.Error())
		// 	return
		// }
		err = subProcess.SetProcessType(ProcessTypeNormal, DbSyncFalse, true) // unless this is a scatter of a scatter...
		if err != nil {
			err = fmt.Errorf("(processInstanceEnQueueScatter) subProcess.SetProcessType returned: %s", err.Error())
			return
		}
		//task
		// create step
		var newProcessStep cwl.WorkflowStep
		//var newTaskStep_in []cwl.WorkflowStepInput
		newProcessStep = templateTaskStep // this should make a copy from template, (this is not a nested copy)

		//fmt.Println("newProcessStep initial:")
		//spew.Dump(newProcessStep)

		// copy scatter inputs

		for inputName := range templateScatterStepIns {
			inputNameBase := path.Base(inputName)
			scatterInput, ok := templateScatterStepIns[inputNameBase]
			if !ok {
				err = fmt.Errorf("(processInstanceEnQueueScatter) %s not in templateScatterStepIns", inputNameBase)
				return
			}

			inputPosition, ok := nameToPostiton[inputNameBase] // inputPosition points to an array of inputs
			if !ok {
				err = fmt.Errorf("(processInstanceEnQueueScatter) %s not in name_to_postiton map", inputNameBase)
				return
			}

			//the_array := scatterInputArrayPtrs[inputPosition]

			theIndex := counter.Counter[inputPosition]
			scatterInput.SourceIndex = theIndex + 1
			newProcessStep.In = append(newProcessStep.In, scatterInput)
		}

		fmt.Println("newProcessStep with everything:")
		spew.Dump(newProcessStep)

		newProcessStep.ID = parentIDStr + "/" + scatterProcessName
		subProcess.SetWorkflowStep(&newProcessStep, true)

		subProcess.SetWorkflowStepID(subProcess, newProcessStep.ID, true)
		children = append(children, scatterProcessName)

		newProcessStep.Scatter = nil // []string{}
		//newScatterTasks = append(newScatterTasks, subTask)
		newScatterProcesses = append(newScatterProcesses, subProcess)
		counterRunning = counter.Increment()
	}

	if task != nil {
		err = task.SetScatterChildren(qm, children, true)
		if err != nil {
			err = fmt.Errorf("(processInstanceEnQueueScatter) task.SetScatterChildren returned: %s", err.Error())
			return
		}
	} else {

	}
	// add tasks to job and submit
	//for i := range newScatterTasks {
	for i := range newScatterProcesses {
		//subTask := newScatterTasks[i]
		subProcess := newScatterProcesses[i]

		//subTaskID, _ := subTask.GetID("processInstanceEnQueueScatter")
		//subTaskIDStr, _ := subTaskID.String()
		subProcessIDStr := subProcess.GetIDStr()

		logger.Debug(3, "(processInstanceEnQueueScatter) adding %s to workflowInstance", subProcessIDStr)

		if task != nil {
			subTask := subProcess.(*Task)

			err = parentWorkflowInstance.AddTask(job, subTask, DbSyncTrue, true)
			if err != nil {
				err = fmt.Errorf("(processInstanceEnQueueScatter) job.AddTask returns: %s", err.Error())
				return
			}

			err = qm.TaskMap.Add(subTask, "processInstanceEnQueueScatter")
			if err != nil {
				//subTaskIDStr := subTask.ID
				err = fmt.Errorf("(processInstanceEnQueueScatter) subProcessIDStr=%s qm.TaskMap.Add() returns: %s", subProcessIDStr, err.Error())
				return
			}

			err = subTask.SetState(TASK_STAT_READY, true, "processInstanceEnQueueScatter")
			if err != nil {
				//subTaskIDStr := subTask.ID
				err = fmt.Errorf("(processInstanceEnQueueScatter) subProcessIDStr=%s subTask.SetState returns: %s", subProcessIDStr, err.Error())
				return
			}
		} else {
			subWorkflowInstance := subProcess.(*WorkflowInstance)

			err = job.AddWorkflowInstance(subWorkflowInstance, true, true)
			if err != nil {
				err = fmt.Errorf("(processInstanceEnQueueScatter) job.AddWorkflowInstance returned: %s", err.Error())
				return
			}

			subWorkflowInstanceID, _ := subWorkflowInstance.GetID(true)

			err = GlobalWorkflowInstanceMap.Add(subWorkflowInstanceID, subWorkflowInstance)
			if err != nil {
				err = fmt.Errorf("(processInstanceEnQueueScatter) GlobalWorkflowInstanceMap.Add returned: %s", err.Error())
				return
			}

		}
	}

	return
}

// happens when task is ready
// prepares task and creates workunits or workflowInstances
// scatter task does not create its own workunit, it just creates new tasks

// workflow: create workflowInstance and tasks
// scatter: create tasks
// commandlinetool: create workunit/s

func (qm *ServerMgr) taskEnQueue(taskID Task_Unique_Identifier, task *Task, job *Job, logTimes bool) (times map[string]time.Duration, err error) {

	taskIDStr, _ := taskID.String()

	var state string
	state, err = task.GetState()
	if err != nil {
		err = fmt.Errorf("(taskEnQueue) Could not get State: %s", err.Error())
		return
	}

	if state != TASK_STAT_READY {
		err = fmt.Errorf("(taskEnQueue) Task state should be TASK_STAT_READY, got state %s", state)
		return
	}

	if task.WorkflowStep != nil {
		logger.Debug(3, "(taskEnQueue) have WorkflowStep")
	} else {
		logger.Debug(3, "(taskEnQueue) DO NOT have WorkflowStep")
	}

	skipWorkunit := false

	var taskType string
	taskType, err = task.GetProcessType()
	if err != nil {
		return
	}

	var notice *Notice
	notice = nil

	logger.Debug(3, "(taskEnQueue) have job.WorkflowContext")

	var workflowInstance *WorkflowInstance
	var workflowInputMap cwl.JobDocMap
	if task.WorkflowInstanceID != "" {
		var ok bool
		workflowInstance, ok, err = job.GetWorkflowInstance(task.WorkflowInstanceID, true)
		if err != nil {
			err = fmt.Errorf("(taskEnQueue) GetWorkflowInstance returned %s", err.Error())
			return
		}
		if !ok {
			err = fmt.Errorf("(taskEnQueue) WorkflowInstance not found: \"%s\"", task.WorkflowInstanceID)
			return
		}

		workflowInputMap = workflowInstance.Inputs.GetMap()
		cwlStep := task.WorkflowStep

		if cwlStep == nil {
			err = fmt.Errorf("(taskEnQueue) task.WorkflowStep is empty")
			return
		}
	}
	//workflow_with_children := false
	//var wfl *cwl.Workflow

	// Detect taskType

	//fmt.Printf("(taskEnQueue) A taskType: %s\n", taskType)

	//if taskType == "" {
	//	err = fmt.Errorf("(taskEnQueue) taskType empty")
	//	return
	//}

	if task.WorkflowInstanceID != "" {
		switch taskType {
		case ProcessTypeScatter:
			logger.Debug(3, "(taskEnQueue) call taskEnQueueScatter")

			if task.WorkflowStep == nil {
				err = fmt.Errorf("(taskEnQueue) task.WorkflowStep == nil")
				return
			}

			notice, err = qm.processInstanceEnQueueScatter(workflowInstance, task, job, workflowInputMap)
			if err != nil {
				err = fmt.Errorf("(taskEnQueue) taskEnQueueScatter returned: %s", err.Error())
				return
			}

		}
	}
	logger.Debug(2, "(taskEnQueue) task %s has type %s", taskIDStr, taskType)
	if taskType == ProcessTypeScatter {
		skipWorkunit = true
	}

	logger.Debug(2, "(taskEnQueue) trying to enqueue task %s", taskIDStr)

	// if task was flagged by resume, recompute, or resubmit - reset it
	err = task.SetResetTask(job.Info)
	if err != nil {
		err = fmt.Errorf("(taskEnQueue) SetResetTask: %s", err.Error())
		return
	}

	inputeStart := time.Now()
	err = qm.locateInputs(task, job) // only old-style AWE
	if logTimes {
		times["locateInputs"] = time.Since(inputeStart)
	}
	if err != nil {
		err = fmt.Errorf("(taskEnQueue) locateInputs: %s", err.Error())
		return
	}

	// init partition
	indexStart := time.Now()

	if task.Inputs != nil && len(task.Inputs) > 0 {
		err = task.InitPartIndex()
		if logTimes {
			times["InitPartIndex"] = time.Since(indexStart)
		}
		if err != nil {
			err = fmt.Errorf("(taskEnQueue) InitPartitionIndex: %s", err.Error())
			return
		}
	}
	outputStart := time.Now()
	err = qm.createOutputNode(task)
	if logTimes {
		times["createOutputNode"] = time.Since(outputStart)
	}
	if err != nil {
		err = fmt.Errorf("(taskEnQueue) createOutputNode: %s", err.Error())
		return
	}

	if !skipWorkunit {
		logger.Debug(3, "(taskEnQueue) create Workunits")
		workunitStart := time.Now()
		var count int
		count, err = qm.CreateAndEnqueueWorkunits(task, job)
		if err != nil {
			err = fmt.Errorf("(taskEnQueue) %s CreateAndEnqueueWorkunits returned: %s", taskIDStr, err.Error())
			return
		}
		logger.Debug(3, "(taskEnQueue) %d Workunits created", count)
		if logTimes {
			times["CreateAndEnqueueWorkunits"] = time.Since(workunitStart)
		}
	}
	err = task.SetState(TASK_STAT_QUEUED, true, "taskEnQueue")
	if err != nil {
		return
	}
	err = task.SetCreatedDate(time.Now()) // TODO: this is pretty stupid and useless. May want to use EnqueueDate here ?
	if err != nil {
		return
	}
	err = task.SetStartedDate(time.Now()) //TODO: will be changed to the time when the first workunit is checked out
	if err != nil {
		return
	}

	//updateStart := time.Now()
	// err = qm.taskCompleted(workflowInstance, task) //task status PENDING->QUEUED
	// if logTimes {
	// 	times["taskCompleted"] = time.Since(updateStart)
	// }
	// if err != nil {
	// 	err = fmt.Errorf("(taskEnQueue) qm.taskCompleted: %s", err.Error())
	// 	return
	// }

	// log event about task enqueue (TQ)
	logger.Event(event.TASK_ENQUEUE, fmt.Sprintf("taskid=%s;totalwork=%d", taskIDStr, task.TotalWork))
	err = qm.CreateTaskPerf(task)
	if err != nil {
		err = fmt.Errorf("(taskEnQueue) CreateTaskPerf returned: %s", err.Error())
		return
	}
	logger.Debug(2, "(taskEnQueue) leaving (task=%s)", taskIDStr)

	if notice != nil {
		// This task is not processed by a worker and thus no notice would be sent. For correct completion the server creates and sends an internal Notice.

		//WorkerId    string                     `bson:"worker_id" json:"worker_id" mapstructure:"worker_id"`
		//Results     *cwl.Job_document          `bson:"results" json:"results" mapstructure:"results"`                            // subset of tool_results with Shock URLs

		qm.feedback <- *notice

	}

	return
}

// invoked by taskEnQueue
// main purpose is to copy output io struct of predecessor task to create the input io structs
func (qm *ServerMgr) locateInputs(task *Task, job *Job) (err error) {
	if task.WorkflowStep != nil && job.WorkflowContext != nil {
		//if job.WorkflowContext.Job_input_map == nil {
		//err = fmt.Errorf("job.WorkflowContext.Job_input_map is empty")
		//	return
		//}
	} else {
		// old AWE-style
		err = task.ValidateInputs(qm)
		if err != nil {
			return
		}
		err = task.ValidatePredata()
		if err != nil {
			err = fmt.Errorf("ValidatePredata failed: %s", err.Error())
		}
	}
	return
}

func (qm *ServerMgr) getCWLSourceArray(workflowInstance *WorkflowInstance, workflowInputMap map[string]cwl.CWLType, job *Job, srcArray []string, errorOnMissingTask bool) (obj cwl.Array, ok bool, err error) {

	obj = cwl.Array{}
	ok = false

	for _, src := range srcArray {
		var element cwl.CWLType
		var src_ok bool
		element, src_ok, _, err = qm.getCWLSource(job, workflowInstance, workflowInputMap, src, errorOnMissingTask, job.WorkflowContext)
		if err != nil {
			err = fmt.Errorf("(getCWLSourceArray) getCWLSource returned: %s", err.Error())
			return
		}
		if !src_ok {
			if errorOnMissingTask {
				err = fmt.Errorf("(getCWLSourceArray) Source %s not found", src)
				return
			}

			// return with ok=false, but do not throw error
			ok = false
			return
		}

		obj = append(obj, element)
	}
	ok = true
	return

}

func (qm *ServerMgr) getCWLSourceFromWorkflowInput(workflowInputMap map[string]cwl.CWLType, srcBase string) (obj cwl.CWLType, reason string, ok bool, err error) {

	//fmt.Println("src_base: " + src_base)
	// search job input
	var thisOK bool
	obj, thisOK = workflowInputMap[srcBase]
	if thisOK {
		//fmt.Println("(getCWLSource) found in workflowInputMap: " + src_base)
		ok = true
		return
	}

	inputsStr := ""
	for key := range workflowInputMap {
		inputsStr += "," + key
	}

	reason = fmt.Sprintf("(getCWLSourceFromWorkflowInput) %s not found, only found: %s", srcBase, inputsStr)

	ok = false
	return

}

func (qm *ServerMgr) getCWLSourceFromStepOutputTool(job *Job, workflowInstance *WorkflowInstance, stepName string, outputName string, errorOnMissingTask bool) (obj cwl.CWLType, ok bool, reason string, err error) {

	// search task and its output
	if workflowInstance.JobID == "" {
		err = fmt.Errorf("(getCWLSourceFromStepOutputTool) workflowInstance.JobId empty")
		return
	}

	if strings.HasPrefix(outputName, "#") {
		err = fmt.Errorf("(getCWLSourceFromStepOutputTool) outputName should not be absolute")
		return
	}

	outputNameBase := path.Base(outputName)
	if outputNameBase != outputName {
		err = fmt.Errorf("(getCWLSourceFromStepOutputTool) outputName is not base")
		return
	}

	//workflowInstanceID, _ := workflowInstance.GetId(true)
	workflowInstanceLocalID := workflowInstance.LocalID

	ancestorTaskNameLocal := workflowInstanceLocalID + "/" + stepName

	ancestorTaskID := Task_Unique_Identifier{}
	ancestorTaskID.JobId = workflowInstance.JobID
	ancestorTaskID.TaskName = ancestorTaskNameLocal

	ancestorTaskIDStr, _ := ancestorTaskID.String()
	var ancestorTask *Task

	logger.Debug(3, "(getCWLSourceFromStepOutputTool) try to find ancester task: %s", ancestorTaskIDStr)

	ancestorTask, ok, err = workflowInstance.GetTask(ancestorTaskID, true)
	if err != nil {
		err = fmt.Errorf("(getCWLSourceFromStepOutputTool) workflowInstance.GetTask returned: %s", err.Error())
		return
	}
	if !ok {

		tasksStr := ""
		if len(workflowInstance.Tasks) > 0 {
			for i := range workflowInstance.Tasks {
				t_str, _ := workflowInstance.Tasks[i].String()
				tasksStr += "," + t_str
			}
		} else {
			tasksStr = "no tasks found"
		}
		reason = fmt.Sprintf("(getCWLSourceFromStepOutputTool) ancestor_task %s not found in workflowInstance %s (tasks found: %s)", ancestorTaskIDStr, workflowInstanceLocalID, tasksStr)

		//spew.Dump(workflowInstance)

		return
	}

	// double check the task is completed
	var ancestorTaskState string
	ancestorTaskState, err = ancestorTask.GetState()
	if err != nil {
		err = fmt.Errorf("(getCWLSourceFromStepOutputTool) ancestorTask.GetState returned: %s", err.Error())
		return
	}
	if ancestorTaskState != TASK_STAT_COMPLETED {
		err = fmt.Errorf("(getCWLSourceFromStepOutputTool) ancestorTaskState != TASK_STAT_COMPLETED, ancestorTaskState=%s", ancestorTaskState)
		return
	}

	obj, ok, reason, err = ancestorTask.GetStepOutput(outputName)
	if err != nil {
		err = fmt.Errorf("(getCWLSourceFromStepOutputTool) ancestor_task.GetStepOutput returned: %s", err.Error())
		return
	}
	if !ok {
		reason = fmt.Sprintf("(getCWLSourceFromStepOutputTool) Output %s (outputName: %s) not found in output of ancestor_task=%s, reason: %s", ancestorTaskNameLocal, outputName, ancestorTaskIDStr, reason)
		return
	}

	return
}

func (qm *ServerMgr) getCWLSourceFromStepOutput_Workflow(job *Job, workflowInstance *WorkflowInstance, step_name string, output_name string, errorOnMissingTask bool) (obj cwl.CWLType, ok bool, reason string, err error) {

	local_name := workflowInstance.LocalID

	subworkflow_name := local_name + "/" + step_name

	var subwi *WorkflowInstance
	subwi, ok, err = job.GetWorkflowInstance(subworkflow_name, true)
	if err != nil {
		err = fmt.Errorf("(getCWLSourceFromStepOutput_Workflow) job.GetWorkflowInstance returned: %s", err.Error())
		return
	}

	if !ok {

		steps := ""
		for _, wi := range job.WorkflowInstancesMap {

			steps += "," + wi.LocalID
		}

		err = fmt.Errorf("(getCWLSourceFromStepOutput_Workflow) step %s (a Workflow) not found (found: %s)", subworkflow_name, steps)
		return
	}

	subwiState, _ := subwi.GetState(true)
	if subwiState != WIStateCompleted {
		msg := fmt.Sprintf("(getCWLSourceFromStepOutput_Workflow) step %s (a WorkflowInstance) is not completed yet (state=%s)", subworkflow_name, subwiState)
		if errorOnMissingTask {
			err = fmt.Errorf(msg)
		} else {
			ok = false
			reason = msg
		}
		return
	}

	//var obj cwl.CWLType
	obj, ok, err = subwi.GetOutput(output_name, true)
	if err != nil {
		err = fmt.Errorf("(getCWLSourceFromStepOutput_Workflow) Could not get output %s in subworkflow %s,  subwi.GetOutput returned: %s", output_name, subworkflow_name, err.Error())
		return
	}

	if !ok {

		outputs := ""
		for i, _ := range subwi.Outputs {
			named := subwi.Outputs[i]
			outputs += "," + named.ID
		}

		err = fmt.Errorf("(getCWLSourceFromStepOutput_Workflow) output %s not found in workflow %s (found: %s)", output_name, subworkflow_name, outputs)
		return
	}

	return
}

// To get StepOutput function has to distinguish between task (CommandLine/Expression-Tool) and Subworkflow
// src = workflow_name / step_name / output_name
func (qm *ServerMgr) getCWLSourceFromStepOutput(job *Job, workflowInstance *WorkflowInstance, stepName string, outputName string, errorOnMissingTask bool) (obj cwl.CWLType, ok bool, reason string, err error) {
	ok = false
	//step_name_abs := workflow_name + "/" + step_name
	workflowInstanceID, _ := workflowInstance.GetID(true)
	//workflowInstanceLocalID := workflowInstance.LocalID

	logger.Debug(3, "(getCWLSourceFromStepOutput) %s / %s (workflowInstanceID: %s)", stepName, outputName, workflowInstanceID)
	_ = workflowInstanceID
	// *** check if workflow_name + "/" + step_name is a subworkflow
	//workflowInstanceName := workflow_name + "/" + step_name

	outputNameBase := path.Base(outputName)
	if outputNameBase != outputName {
		err = fmt.Errorf("(getCWLSourceFromStepOutput) outputNameBase != outputName (outputNameBase=%s, outputName=%s)", outputNameBase, outputName)
		return
	}

	//var wi *WorkflowInstance

	//wi, ok, err = job.GetWorkflowInstance(workflowInstanceName, true)
	//if err != nil {
	//	err = fmt.Errorf("(getCWLSourceFromStepOutput) job.GetWorkflowInstance returned: %s", err.Error())
	//	return
	//}

	//if ok {
	ok = false

	context := job.WorkflowContext

	// get workflow
	var workflow *cwl.Workflow
	workflow, err = workflowInstance.GetWorkflow(context)
	if err != nil {
		err = fmt.Errorf("(getCWLSourceFromStepOutput) workflowInstance.GetWorkflow returned: %s", err.Error())
		return
	}

	var step *cwl.WorkflowStep
	step, err = workflow.GetStep(stepName)
	if err != nil {

		steps := ""
		for i, _ := range workflow.Steps {
			s2 := &workflow.Steps[i]
			steps += "," + s2.ID
		}

		err = fmt.Errorf("(getCWLSourceFromStepOutput) Step %s not found (found %s)", stepName, steps)
		return
	}

	// determinre process type
	var process_type string
	process_type, err = step.GetProcessType(context)
	if err != nil {
		err = fmt.Errorf("(getCWLSourceFromStepOutput) step.GetProcessType returned: %s", err.Error())
		return
	}

	switch process_type {
	case "CommandLineTool", "ExpressionTool":
		obj, ok, reason, err = qm.getCWLSourceFromStepOutputTool(job, workflowInstance, stepName, outputName, errorOnMissingTask)
		if err != nil {
			err = fmt.Errorf("(getCWLSourceFromStepOutput) getCWLSourceFromStepOutput_Tool returned: %s", err.Error())
			return
		}
	case "Workflow":
		obj, ok, reason, err = qm.getCWLSourceFromStepOutput_Workflow(job, workflowInstance, stepName, outputName, errorOnMissingTask)
		if err != nil {
			err = fmt.Errorf("(getCWLSourceFromStepOutput) getCWLSourceFromStepOutput_Workflow returned: %s", err.Error())
			return
		}
	default:
		err = fmt.Errorf("(getCWLSourceFromStepOutput) process_type %s unknown", process_type)
		return
	}

	//err = fmt.Errorf("(getCWLSourceFromStepOutput) not a subworkflow")
	return

}

// GetSourceFromWorkflowInstanceInput _
func (qm *ServerMgr) GetSourceFromWorkflowInstanceInput(workflowInstance *WorkflowInstance, src string, context *cwl.WorkflowContext, errorOnMissingTask bool) (obj cwl.CWLType, ok bool, reason string, err error) {

	ok = false

	fmt.Printf("(GetSourceFromWorkflowInstanceInput) src: %s\n", src)
	srcBase := path.Base(src)

	fmt.Printf("(GetSourceFromWorkflowInstanceInput) src_base: %s\n", srcBase)
	srcPath := strings.TrimSuffix(src, "/"+srcBase)

	//srcArray := strings.Split(src, "/")
	//src_base := srcArray[1]

	if workflowInstance == nil {
		err = fmt.Errorf("(GetSourceFromWorkflowInstanceInput) workflowInstance==nil (src: %s)", src)
		return
	}

	if workflowInstance.Inputs == nil {
		if errorOnMissingTask {

			err = fmt.Errorf("(GetSourceFromWorkflowInstanceInput) workflowInstance.Inputs empty (src: %s)", src)
			return
		}

		err = nil
		reason = fmt.Sprintf("(GetSourceFromWorkflowInstanceInput) workflowInstance.Inputs empty (src: %s)", src)
		logger.Debug(3, reason)
		ok = false

		return
	}

	// search for input object
	obj, ok = workflowInstance.Inputs.Get(srcBase)
	if ok {
		ok = true
		return
	}

	// did not find input object, get InputParameter to check if input is optional
	var workflow *cwl.Workflow
	workflow, err = workflowInstance.GetWorkflow(context)
	if err != nil {
		err = fmt.Errorf("(GetSourceFromWorkflowInstanceInput) workflowInstance.GetWorkflow returned: %s", err.Error())
		return
	}

	// find InputParameter
	var inputParameter *cwl.InputParameter
	inputParameter = nil
	for i := range workflow.Inputs {
		inp := &workflow.Inputs[i]
		if path.Base(inp.ID) == srcBase {
			inputParameter = inp
			break
		}
	}
	if inputParameter == nil {
		fmt.Printf("(GetSourceFromWorkflowInstanceInput) InputParameters: %d \n", len(workflow.Inputs))
		for i := range workflow.Inputs {
			inp := &workflow.Inputs[i]
			fmt.Printf("(GetSourceFromWorkflowInstanceInput) InputParameter: %s \n", inp.ID)
		}

		err = fmt.Errorf("(GetSourceFromWorkflowInstanceInput) InputParameter for %s not found", srcBase)
		return
	}

	// check if input is optional
	optional := false

	var inputParameterTypes []cwl.CWLType_Type

	inputParameterTypesIf := inputParameter.Type
	switch inputParameterTypesIf.(type) {
	case []interface{}:
		inputParameterTypesArrayIf := inputParameterTypesIf.([]interface{})

		for _, tIf := range inputParameterTypesArrayIf {
			var t cwl.CWLType_Type
			t, ok = tIf.(cwl.CWLType_Type)
			if !ok {
				err = fmt.Errorf("(GetSourceFromWorkflowInstanceInput) Could not convert array element type")
				return
			}
			inputParameterTypes = append(inputParameterTypes, t)
		}

	default:
		var t cwl.CWLType_Type
		t, ok = inputParameterTypesIf.(cwl.CWLType_Type)
		if !ok {
			err = fmt.Errorf("(GetSourceFromWorkflowInstanceInput) Could not element type")
			return
		}
		inputParameterTypes = []cwl.CWLType_Type{t}
	}

	for _, inputType := range inputParameterTypes {
		if inputType == cwl.CWLNull {
			optional = true
			break
		}
	}

	if optional {
		reason = "optional"
		ok = false
		return
	}

	if errorOnMissingTask {
		//fmt.Println("workflowInstance.Inputs:")
		//spew.Dump(workflowInstance.Inputs)
		//	panic("output not found a)")

		err = fmt.Errorf("(GetSourceFromWorkflowInstanceInput) found ancestor_task %s, but output %s not found in workflowInstance.Inputs (was %s)", srcPath, srcBase, src)
		return
	}

	msg := fmt.Sprintf("(GetSourceFromWorkflowInstanceInput) found ancestor_task %s, but output %s not found in workflowInstance.Inputs", srcPath, srcBase)
	logger.Debug(3, msg)
	ok = false
	reason = msg

	return

}

// isSourceGeneratorReady srcGenerator is either the workflow or a workflow step
// a workflow step calls a Tool of a Workflow
func (qm *ServerMgr) isSourceGeneratorReady(job *Job, workflowInstance *WorkflowInstance, srcGenerator string, errorOnMissingTask bool, context *cwl.WorkflowContext) (ok bool, reason string, err error) {

	ok = false
	//src = strings.TrimPrefix(src, "#entrypoint/")
	logger.Debug(3, "(isSourceGeneratorReady) start, srcGenerator: %s", srcGenerator)

	if srcGenerator == "" {
		err = fmt.Errorf("(isSourceGeneratorReady) srcGenerator string empty")
		return
	}
	if srcGenerator == "." {
		// must be a workflow input
		logger.Debug(3, "(isSourceGeneratorReady) must be a workflow input, srcGeneratorAbs: %s", srcGenerator)
		ok = true
		return
	}

	srcGeneratorArray := strings.Split(srcGenerator, "/")
	var srcGeneratorArrayNew []string
	for i := range srcGeneratorArray {
		if len(srcGeneratorArray[i]) != 36 { // TODO uuid test
			srcGeneratorArrayNew = append(srcGeneratorArrayNew, srcGeneratorArray[i])
		}

	}
	srcGeneratorArray = srcGeneratorArrayNew
	srcGenerator = strings.Join(srcGeneratorArray, "/")

	logger.Debug(3, "(isSourceGeneratorReady) srcGenerator: %s  vs workflowInstance: %s", srcGenerator, workflowInstance.LocalID)
	if srcGenerator == workflowInstance.LocalID {
		// must be a workflow input
		ok = true
		return
	}
	// must be a step output
	// next: find step, find process

	srcGeneratorAbs := ""
	if strings.HasPrefix(srcGenerator, "#") {
		srcGeneratorAbs = srcGenerator
	} else {
		srcGeneratorAbs = path.Join(workflowInstance.LocalID, srcGenerator)
	}

	srcArray := strings.Split(srcGeneratorAbs, "/")

	if len(srcArray) == 1 {
		// must be a workflow input
		ok = true
		return
	}

	// get workflow object and find step

	var workflow *cwl.Workflow
	workflow, err = workflowInstance.GetWorkflow(context)
	if err != nil {
		err = fmt.Errorf("(isSourceGeneratorReady) workflowInstance.GetWorkflow returned: %s", err.Error())
		return
	}

	srcGeneratorBase := path.Base(srcGenerator)

	var workflowStep *cwl.WorkflowStep
	workflowStep, err = workflow.GetStep(srcGeneratorBase)
	if err != nil {
		err = fmt.Errorf("(isSourceGeneratorReady) workflow.GetStep(%s) (with srcGenerator: %s) returned: %s", srcGeneratorBase, srcGenerator, err.Error())
		return
	}

	// got step, now detect the type of process and check state of Task or WorkflowInstance

	var processType string
	processType, err = workflowStep.GetProcessType(context)

	switch processType {

	case "CommandLineTool", "ExpressionTool":
		var task *Task
		var taskFound bool
		task, taskFound, err = workflowInstance.GetTaskByName(srcGeneratorBase, true)
		if err != nil {
			err = fmt.Errorf("(isSourceGeneratorReady) workflowInstance.GetTaskByName returned: %s", err.Error())
			return
		}
		if !taskFound {

			if errorOnMissingTask {
				err = fmt.Errorf("(isSourceGeneratorReady) task %s not found", srcGeneratorAbs)
				return
			}

			reason = fmt.Sprintf("(isSourceGeneratorReady) task %s not found", srcGeneratorAbs)
			return

		}
		var taskState string
		taskState, err = task.GetState()
		if err != nil {
			return
		}
		if taskState != TASK_STAT_COMPLETED {

			taskID, _ := task.GetID("isSourceGeneratorReady")
			taskIDStr, _ := taskID.String()
			reason = fmt.Sprintf("(isSourceGeneratorReady) dependent task %s has state %s (srcGenerator=%s)", taskIDStr, taskState, srcGenerator)
			logger.Debug(3, "(isSourceGeneratorReady) srcGenerator not ready: %s (reason=%s)", srcGenerator, reason)
		} else {
			logger.Debug(3, "(isSourceGeneratorReady) ready: %s", srcGenerator)
			ok = true
		}

		return

	case "Workflow":

		var otherWorkflowInstance *WorkflowInstance

		otherWorkflowInstance, ok, err = job.GetWorkflowInstance(srcGeneratorAbs, true)
		if err != nil {
			err = fmt.Errorf("(isSourceGeneratorReady) job.GetWorkflowInstance returned: %s", err.Error())
			return
		}
		if !ok {
			reason = fmt.Sprintf("(isSourceGeneratorReady) job did not contain workflowInstance %s", srcGeneratorAbs)
			return
		}

		otherWorkflowInstanceState, _ := otherWorkflowInstance.GetState(true)

		if otherWorkflowInstanceState == WIStateCompleted {
			ok = true
			logger.Debug(3, "(isSourceGeneratorReady) WI ready: %s", srcGenerator)
			return
		}
		ok = false
		reason = "(isSourceGeneratorReady) workfow instance is not completed"
		logger.Debug(3, "(isSourceGeneratorReady) WI not ready: %s", srcGenerator)
		return

	default:
		err = fmt.Errorf("(isSourceGeneratorReady) processType %s unkown", processType)

	}

	return

}

// getCWLSource retrieves the input from either the (sub-)workflow input, or from the output of another task in the same (sub-)workflow
// errorOnMissingTask: when checking if a task is ready, a missing task is not an error, it just means task is not ready,
//    but when getting data this is actually an error.
// src format: #workflow/step/step_output or #workflow/input
//             or step/step_output or input
func (qm *ServerMgr) getCWLSource(job *Job, workflowInstance *WorkflowInstance, workflowInputMap map[string]cwl.CWLType, src string, errorOnMissingTask bool, context *cwl.WorkflowContext) (obj cwl.CWLType, ok bool, reason string, err error) {

	ok = false

	original_src := src

	srcArray := strings.Split(src, "/")

	if strings.HasPrefix(src, "#") || len(srcArray[0]) == 36 { // TODO very ugly
		srcArray = srcArray[1:] // remove prefix
	}
	src = strings.Join(srcArray, "/")

	logger.Debug(3, "(getCWLSource) src: %s", src)

	var stepReason string
	logger.Debug(3, "(getCWLSource) len(srcArray): %d", len(srcArray))

	srcArrayLen := len(srcArray)
	if srcArrayLen > 3 {
		// case: workflow/step/output
		if len(srcArray[srcArrayLen-3]) == 36 {
			srcArray = []string{srcArray[srcArrayLen-2], srcArray[srcArrayLen-1]}
		} else if len(srcArray[srcArrayLen-2]) == 36 {
			// case: workflow/input

			srcArray = []string{srcArray[srcArrayLen-1]}

		}
		logger.Debug(3, "(getCWLSource) old src: %s", src)
		src = strings.Join(srcArray, "/")
		logger.Debug(3, "(getCWLSource) new src: %s", src)
		srcArrayLen = len(srcArray)
	}

	if srcArrayLen == 1 {

		// must be workflow input
		obj, ok, stepReason, err = qm.GetSourceFromWorkflowInstanceInput(workflowInstance, src, context, errorOnMissingTask)

		if err != nil {
			err = fmt.Errorf("(getCWLSource) GetSourceFromWorkflowInstanceInput returned: %s", err.Error())
			return
		}
		if !ok {
			reason = "GetSourceFromWorkflowInstanceInput returned: " + stepReason
		}

		return
	}

	//if len(srcArray) == 3 {
	//	if len(srcArray[1]) == 36 { // handle special case of embedded workflow
	//		srcArray = []string{srcArray[0], srcArray[2]}
	//	}
	//}

	if srcArrayLen != 2 {
		err = fmt.Errorf("(getCWLSource) len(srcArray) != 2  (%d) (src: %s)", len(srcArray), src)
		return
	}

	/// must be a step output

	//workflow_name := strings.Join(srcArray[0:len(srcArray)-3], "/")
	fmt.Printf("(getCWLSource) srcArrayLen: %d\n", srcArrayLen)
	fmt.Printf("(getCWLSource) src: %s\n", src)
	fmt.Printf("(getCWLSource) original_src: %s\n", original_src)

	var stepName string
	if len(srcArray[srcArrayLen-2]) == 36 {
		err = fmt.Errorf("nope")
		return
		stepName = srcArray[srcArrayLen-3]
	} else {
		stepName = srcArray[srcArrayLen-2]
	}
	outputName := srcArray[srcArrayLen-1]

	logger.Debug(3, "(getCWLSource) call getCWLSourceFromStepOutput: stepName=%s, outputName=%s", stepName, outputName)

	//var step_reason string
	obj, ok, stepReason, err = qm.getCWLSourceFromStepOutput(job, workflowInstance, stepName, outputName, errorOnMissingTask)
	if err != nil {
		err = fmt.Errorf("(getCWLSource) (stepName=%s, outputName=%s) getCWLSourceFromStepOutput returned: %s", stepName, outputName, err.Error())
		return
	}
	if !ok {
		reason = "(getCWLSource) getCWLSourceFromStepOutput returned: " + stepReason
	}
	return

	// //srcBase := path.Base(src)

	// //src = strings.TrimPrefix(src, "#main/")

	// var genericObject cwl.CWLObject
	// genericObject, ok, err = context.Get(src, true)
	// if err != nil {
	// 	err = fmt.Errorf("(getCWLSource) context.Get returned: %s", err.Error())
	// 	return
	// }
	// if !ok {
	// 	reason = fmt.Sprintf("(getCWLSource) context.All did not contain %s", src)
	// 	return
	// }

	// typeStr := fmt.Sprintf("%s", reflect.TypeOf(genericObject))

	// // var type_str string
	// // type_str, err = context.GetType(src)
	// // if err != nil {
	// // 	err = fmt.Errorf("(getCWLSource) context.GetType returned: %s", err.Error())
	// // 	return
	// // }

	// logger.Debug(3, "(getCWLSource) searching for %s (type: %s)", src, typeStr)

	// switch typeStr {
	// case "*cwl.InputParameter", "*cwl.CommandInputParameter": // CommandInputParameter is from CommandLineTool, InputParameter from ExpressionTool
	// 	logger.Debug(3, "(getCWLSource) a workflow input")
	// 	// must be a workflow input, e.g. #main/jobid (workflow, input)
	// 	//src_base := srcArray[1]

	// 	obj, ok, reason, err = qm.GetSourceFromWorkflowInstanceInput(workflowInstance, src, context, errorOnMissingTask)

	// 	if err != nil {
	// 		err = fmt.Errorf("(getCWLSource) GetSourceFromWorkflowInstanceInput returned: %s", err.Error())
	// 		return
	// 	}

	// 	//if !ok {

	// 	//	fmt.Printf("workflowInstance: (looking for %s)\n", src)
	// 	//	spew.Dump(workflowInstance)
	// 	//	panic("done")
	// 	//}

	// 	return

	// case "*cwl.WorkflowStepOutput":
	// 	logger.Debug(3, "(getCWLSource) a step output")
	// 	// must be a step output, e.g. #main/filter/rejected (workflow, step, output)
	// 	//workflow_name := strings.Join(srcArray[0:len(srcArray)-3], "/")
	// 	step_name := srcArray[len(srcArray)-2]
	// 	output_name := srcArray[len(srcArray)-1]

	// 	var step_reason string
	// 	obj, ok, step_reason, err = qm.getCWLSourceFromStepOutput(job, workflowInstance, step_name, output_name, errorOnMissingTask)
	// 	if err != nil {
	// 		err = fmt.Errorf("(getCWLSource) ( %s, %s) getCWLSourceFromStepOutput returned: %s", step_name, output_name, err.Error())
	// 		return
	// 	}
	// 	if !ok {
	// 		reason = "getCWLSourceFromStepOutput returned: " + step_reason
	// 	}
	// 	return

	// case "*cwl.File":
	// 	thing := genericObject.(*cwl.File)

	// 	obj = thing
	// 	return
	// case "*cwl.Array":
	// 	thing := genericObject.(*cwl.Array)

	// 	obj = thing
	// 	return
	// case "*cwl.String":
	// 	thing := genericObject.(*cwl.String)

	// 	obj = thing
	// 	return
	// case "*cwl.Int":
	// 	thing := genericObject.(*cwl.Int)

	// 	obj = thing
	// 	return
	// case "*cwl.Double":
	// 	thing := genericObject.(*cwl.Double)

	// 	obj = thing
	// 	return
	// case "*cwl.Record":
	// 	thing := genericObject.(*cwl.Record)

	// 	obj = thing
	// 	return
	// case "*cwl.Boolean":
	// 	thing := genericObject.(*cwl.Boolean)

	// 	obj = thing
	// 	return
	// case "*cwl.Null":
	// 	thing := genericObject.(*cwl.Null)

	// 	obj = thing
	// 	return
	// }

	// err = fmt.Errorf("(getCWLSource) could not parse source: %s, type %s unknown", src, typeStr)

	// return

}

// GetDependencies Tasks or Subworkflows
func (qm *ServerMgr) GetDependencies(job *Job, workflowInstance *WorkflowInstance, workflowInputMap map[string]cwl.CWLType, workflow_step *cwl.WorkflowStep, context *cwl.WorkflowContext) (err error) {
	if workflow_step.In == nil {
		return
	}

	if len(workflow_step.In) == 0 {

		return
	}
	var ok bool
	src_str := ""

	for _, input := range workflow_step.In {

		id := input.ID
		fmt.Printf("(GetDependencies) id: %s\n", id)

		if input.Source != nil {

			source_is_array := false

			source_as_string := ""
			source_as_array, source_is_array := input.Source.([]interface{})

			if source_is_array {
				fmt.Printf("(GetDependencies) source is a array: %s", spew.Sdump(input.Source))
				if input.SourceIndex != 0 {
					// from scatter step
					// fmt.Printf("source is a array with Source_index: %s", spew.Sdump(input.Source))
					if input.SourceIndex > len(source_as_array) {
						err = fmt.Errorf("(GetStepInputObjects) input.SourceIndex >= len(source_as_array) %d > %d", input.SourceIndex, len(source_as_array))
						return
					}
					src := source_as_array[input.SourceIndex-1]
					var src_str string
					//var ok bool
					src_str, ok = src.(string)
					if !ok {
						err = fmt.Errorf("src is not a string")
						return
					}
					_ = src_str
					panic("A1")
				} else {
					cwl_array := cwl.Array{}
					for _, src := range source_as_array { // usually only one
						fmt.Println("src: " + spew.Sdump(src))
						var src_str string
						//var ok bool
						src_str, ok = src.(string)
						if !ok {
							err = fmt.Errorf("src is not a string")
							return
						}
						_ = src_str
					}
					_ = cwl_array
					panic("A2")
				}
				panic("A")

			} else { // NOT source_is_array
				source_as_string, ok = input.Source.(string)
				if !ok {
					err = fmt.Errorf("(GetStepInputObjects) (string) Cannot parse WorkflowStep source: %s", spew.Sdump(input.Source))
					return
				}
				panic("B")
			}
			_ = source_as_string
			_ = src_str
		} else { // input.Source == nil
			if input.Default == nil && input.ValueFrom == "" {
				err = fmt.Errorf("(GetStepInputObjects) A) sorry, source, Default and ValueFrom are missing") // TODO StepInputExpressionRequirement
				return
			}

			if input.Default != nil {
				panic("D")
				// var default_value cwl.CWLType
				// default_value, err = cwl.NewCWLType(cmd_id, input.Default, context)
				// if err != nil {
				// 	err = fmt.Errorf("(GetStepInputObjects) NewCWLTypeFromInterface(input.Default) returns: %s", err.Error())
				// 	return
				// }

				// if default_value == nil {
				// 	err = fmt.Errorf("(GetStepInputObjects) default_value == nil ")
				// 	return
				// }

				// workunit_input_map[cmd_id] = default_value
			}
			panic("C")
		} // end if input.Source

	} // end for

	return
}

// GetStepInputObject called by GetStepInputObjects
func (qm *ServerMgr) GetStepInputObject(job *Job, workflowInstance *WorkflowInstance, workflowInputMap map[string]cwl.CWLType, workunitInputMap cwl.JobDocMap, inputI int, input *cwl.WorkflowStepInput, context *cwl.WorkflowContext) (ok bool, reason string, err error) {

	//spew.Dump(workflowStep.In)

	id := input.ID
	fmt.Printf("(GetStepInputObject) workflow_step.In: (%d, %s)\n", inputI, id)
	//	fmt.Println("(GetStepInputObjects) id: %s", id)
	cmdID := path.Base(id)

	workflowInstanceLocalID := workflowInstance.LocalID
	// get data from Source, Default or valueFrom

	linkMergeMethod := ""
	if input.LinkMerge != nil {
		linkMergeMethod = string(*input.LinkMerge)
	} else {
		// default: merge_nested
		linkMergeMethod = "merge_nested"
	}

	if input.Source != nil {
		fmt.Println("(GetStepInputObject) input.Source != nil")
		//source_object_array := []cwl.CWLType{}
		//resolve pointers in source

		sourceIsArray := false

		//sourceAsString := ""
		sourceAsArray, sourceIsArray := input.Source.([]interface{})

		if sourceIsArray {
			fmt.Printf("(GetStepInputObject) source is a array: %s", spew.Sdump(input.Source))

			if input.SourceIndex != 0 {
				// from scatter step
				// fmt.Printf("source is a array with Source_index: %s", spew.Sdump(input.Source))
				if input.SourceIndex > len(sourceAsArray) {
					err = fmt.Errorf("(GetStepInputObject) input.SourceIndex >= len(sourceAsArray) %d > %d", input.SourceIndex, len(sourceAsArray))
					return
				}
				src := sourceAsArray[input.SourceIndex-1]
				var srcStr string
				//var ok bool
				srcStr, ok = src.(string)
				if !ok {
					err = fmt.Errorf("src is not a string")
					return
				}
				var jobObj cwl.CWLType
				jobObj, ok, _, err = qm.getCWLSource(job, workflowInstance, workflowInputMap, srcStr, true, job.WorkflowContext)
				if err != nil {
					err = fmt.Errorf("(GetStepInputObject) (array) getCWLSource returns: %s", err.Error())
					return
				}
				if !ok {
					err = fmt.Errorf("(GetStepInputObject) (array) getCWLSource did not find output \"%s\"", srcStr)
					return // TODO allow optional ??
				}

				workunitInputMap[cmdID] = jobObj
			} else {
				// case Source_index == 0

				cwlArray := cwl.Array{}
				for _, src := range sourceAsArray { // usually only one
					fmt.Println("src: " + spew.Sdump(src))
					var srcStr string
					//var ok bool
					srcStr, ok = src.(string)
					if !ok {
						err = fmt.Errorf("src is not a string")
						return
					}

					// if ...
					//embedded_workflowInstanceID := "_root/" + strings.Join(srcArray[1:len(srcArray)-2], "/")

					var jobObj cwl.CWLType
					jobObj, ok, _, err = qm.getCWLSource(job, workflowInstance, workflowInputMap, srcStr, true, job.WorkflowContext)
					if err != nil {
						err = fmt.Errorf("(GetStepInputObject) (array) getCWLSource returns: %s", err.Error())
						return
					}
					if !ok {
						err = fmt.Errorf("(GetStepInputObject) (array) getCWLSource did not find output \"%s\"", srcStr)
						return // TODO allow optional ??
					}

					if linkMergeMethod == "merge_flattened" {

						job_obj_type := jobObj.GetType()

						if job_obj_type != cwl.CWLArray {
							err = fmt.Errorf("(GetStepInputObject) merge_flattened, expected array as input, but got %s", job_obj_type)
							return
						}

						var an_array *cwl.Array
						an_array, ok = jobObj.(*cwl.Array)
						if !ok {
							err = fmt.Errorf("got type: %s", reflect.TypeOf(jobObj))
							return
						}

						for i, _ := range *an_array {
							//source_object_array = append(source_object_array, (*an_array)[i])
							cwlArray = append(cwlArray, (*an_array)[i])
						}

					} else if linkMergeMethod == "merge_nested" {
						//source_object_array = append(source_object_array, job_obj)
						cwlArray = append(cwlArray, jobObj)
					} else {
						err = fmt.Errorf("(GetStepInputObject) link_merge_method %s not supported", linkMergeMethod)
						return
					}
					//cwl_array = append(cwl_array, obj)
				}

				workunitInputMap[cmdID] = &cwlArray

			}
		} else {
			fmt.Printf("(GetStepInputObject) source is NOT a array: %s", spew.Sdump(input.Source))
			//var ok bool
			var sourceAsStringRaw string
			sourceAsStringRaw, ok = input.Source.(string)
			if !ok {
				err = fmt.Errorf("(GetStepInputObject) (string) Cannot parse WorkflowStep source: %s", spew.Sdump(input.Source))
				return
			}

			logger.Debug(3, "(GetStepInputObject) sourceAsStringRaw: %s", sourceAsStringRaw)

			sourceAsStringRawArray := strings.Split(sourceAsStringRaw, "/")

			isAbs := false
			if strings.HasPrefix(sourceAsStringRaw, "#") {
				isAbs = true
			}

			stepName := ""
			outputName := ""
			inputName := ""

			sourceAsString := ""
			inputType := ""
			if !isAbs {
				//logger.Debug(3, "(GetStepInputObject) !isAbs sourceAsStringRaw: %s", sourceAsStringRaw)
				if len(sourceAsStringRawArray) == 1 {
					inputType = "workflowInput"
					inputName = sourceAsStringRawArray[0]
				} else if len(sourceAsStringRawArray) == 2 {
					inputType = "stepOutput"
					stepName = sourceAsStringRawArray[0]
					outputName = sourceAsStringRawArray[1]
				} else {
					err = fmt.Errorf("(GetStepInputObject) !isAbs , cannot parse sourceAsStringRaw: %s (workflowInstanceLocalID: %s)", sourceAsStringRaw, workflowInstanceLocalID)
					return
				}
				sourceAsString = path.Join(workflowInstance.LocalID, sourceAsStringRaw)
			} else {

				if !strings.HasPrefix(sourceAsStringRaw, workflowInstanceLocalID+"/") {
					err = fmt.Errorf("(GetStepInputObject) isAbs %s is not a prefix of %s ", workflowInstanceLocalID, sourceAsStringRaw)
					return
				}

				withoutPrefix := strings.TrimPrefix(sourceAsStringRaw, workflowInstanceLocalID+"/")
				logger.Debug(3, "(GetStepInputObject) isAbs , withoutPrefix: %s (workflowInstanceLocalID: %s)", withoutPrefix, workflowInstanceLocalID)
				withoutPrefixArray := strings.Split(withoutPrefix, "/")

				//test := uuid.Parse(withoutPrefixArray[0])
				if uuid.Parse(withoutPrefixArray[0]) != nil {
					withoutPrefixArray = withoutPrefixArray[1:]
				}

				if len(withoutPrefixArray) == 1 { // e.g. file1
					inputType = "workflowInput"
					inputName = withoutPrefixArray[0]
				} else if len(withoutPrefixArray) == 2 {
					//logger.Debug(3, "(GetStepInputObject) isAbs len2, withoutPrefix: %s", withoutPrefix)
					inputType = "stepOutput"
					stepName = withoutPrefixArray[0]
					outputName = withoutPrefixArray[1]
				} else {
					err = fmt.Errorf("(GetStepInputObject) isAbs , cannot parse sourceAsStringRaw: %s withoutPrefix: %s (workflowInstanceLocalID: %s)", sourceAsStringRaw, withoutPrefix, workflowInstanceLocalID)
					return
				}

				sourceAsString = sourceAsStringRaw
			}
			logger.Debug(3, "(GetStepInputObject) determined inputType: %s", inputType)
			logger.Debug(3, "(GetStepInputObject) sourceAsString: %s", sourceAsString)

			sourceAsStringArray := strings.Split(sourceAsString, "/")

			sourceAsStringArrayLen := len(sourceAsStringArray)
			sourceAsStringRawArrayLen := len(sourceAsStringRawArray)
			logger.Debug(3, "(GetStepInputObject) sourceAsStringArrayLen: %d , sourceAsStringRawArrayLen: %d", sourceAsStringArrayLen, sourceAsStringRawArrayLen)

			functionStr := ""

			var jobObj cwl.CWLType
			var foundJobObj bool

			// #main/step/output ,  step/output , #main/input, input, #main/step/<uuid>/input, #main/step/<uuid>/step/output

			if inputType == "stepOutput" { //main/step/output // this must be step output, not a workflow input
				logger.Debug(3, "(GetStepInputObject) must be a step output: %s (%s)", sourceAsStringRaw, sourceAsString)
				// example: #main/step1/cff5b4fa-86b6-4d6e-ac95-3548f6a839cf/file1

				// if len(sourceAsStringArray[sourceAsStringArrayLen-3]) == 36 {
				// 	// sepcial case for uuid-emedded workflows
				// 	sourceAsStringArray = []string{sourceAsStringArray[sourceAsStringArrayLen-3], sourceAsStringArray[sourceAsStringArrayLen-2], sourceAsStringArray[sourceAsStringArrayLen-1]}
				// } else if len(sourceAsStringArray[sourceAsStringArrayLen-2]) == 36 {
				// 	// case: workflow/input

				// 	sourceAsStringArray = []string{sourceAsStringArray[sourceAsStringArrayLen-2], sourceAsStringArray[sourceAsStringArrayLen-1]}

				// }
				// logger.Debug(3, "(GetStepInputObject) old sourceAsString: %s", sourceAsString)
				// sourceAsString = "#" + strings.Join(sourceAsStringArray, "/")
				// logger.Debug(3, "(GetStepInputObject) new sourceAsString: %s", sourceAsString)

				functionStr = "getCWLSourceFromStepOutput"
				var stepReason string
				jobObj, foundJobObj, stepReason, err = qm.getCWLSourceFromStepOutput(job, workflowInstance, stepName, outputName, true)
				if err != nil {
					err = fmt.Errorf("(GetStepInputObject) (inputType=%s, stepName=%s, outputName=%s) getCWLSourceFromStepOutput returned: %s (stepReason=%s)", inputType, stepName, outputName, err.Error(), stepReason)
					return
				}

			} else {
				// must be workflowInput

				logger.Debug(3, "(GetStepInputObject) must be a workflow input: %s (sourceAsString=%s, )", inputName, sourceAsString)
				functionStr = "getCWLSourceFromWorkflowInput"
				jobObj, reason, foundJobObj, err = qm.getCWLSourceFromWorkflowInput(workflowInputMap, inputName)
				if err != nil {
					err = fmt.Errorf("(GetStepInputObject) (sourceAsStringRaw: %s ) getCWLSourceFromWorkflowInput returns: %s", sourceAsStringRaw, err.Error())
					return
				}

			}

			if !foundJobObj {
				logger.Debug(3, "(GetStepInputObject) source_as_string %s not found by %s, reason: %s", sourceAsString, functionStr, reason)

				logger.Debug(3, "(GetStepInputObject) qm.getCWLSource did not return an object (reason: %s), now check input.Default", reason)
				if input.Default == nil {
					//logger.Debug(1, "(GetStepInputObject) (string) getCWLSource did not find output (nor a default) that can be used as input \"%s\"", sourceAsString)

					if !strings.HasSuffix(reason, "optional") {

						ok = false
						reason = fmt.Sprintf("(GetStepInputObject) %s did not find source %s and has no Default (reason: %s)", functionStr, sourceAsString, reason)
						//err = fmt.Errorf("(GetStepInputObject) getCWLSource did not find source %s and has no Default (reason: %s)", source_as_string, reason)
						return
					}
				}
				//logger.Debug(1, "(GetStepInputObject) (string) getCWLSource found something \"%s\"", sourceAsString)
				jobObj, err = cwl.NewCWLType("", "", input.Default, context)
				if err != nil {
					err = fmt.Errorf("(GetStepInputObject) could not use default: %s", err.Error())
					return
				}
			}
			//fmt.Println("(GetStepInputObject) got a input.Default")
			//spew.Dump(job_obj)

			//fmt.Printf("(GetStepInputObject) SourceIndex: %d\n", input.SourceIndex)
			if input.SourceIndex != 0 {
				realSourceIndex := input.SourceIndex - 1

				var jobObjArrayPtr *cwl.Array
				jobObjArrayPtr, ok = jobObj.(*cwl.Array)
				if !ok {
					err = fmt.Errorf("(GetStepInputObject) Array expected but got: %s", reflect.TypeOf(jobObj))
					return
				}
				var jobObjArray cwl.Array
				jobObjArray = *jobObjArrayPtr

				if realSourceIndex >= len(jobObjArray) {
					err = fmt.Errorf("(GetStepInputObject) SourceIndex %d out of bounds, array length: %d", realSourceIndex, len(jobObjArray))
					return
				}

				var element cwl.CWLType
				element = jobObjArray[realSourceIndex]
				//fmt.Printf("(GetStepInputObject) cmd_id=%s element=%s real_source_index=%d\n", cmd_id, element, real_source_index)
				workunitInputMap[cmdID] = element
			} else {
				workunitInputMap[cmdID] = jobObj
			}
		}

	}

	inputObject, hasInput := workunitInputMap[cmdID]

	if hasInput {
		inputObjectType := inputObject.GetType()

		if inputObjectType == cwl.CWLNull {
			hasInput = false
		}
	}

	// fmt.Printf("(GetStepInputObject) check source\n")
	// if input.Source == nil {
	// 	fmt.Printf("(GetStepInputObject) no source\n")
	// } else {
	// 	fmt.Printf("(GetStepInputObject) got source\n")
	// }

	// if hasInput {
	// 	fmt.Printf("(GetStepInputObject) hasInput\nn")
	// } else {
	// 	fmt.Printf("(GetStepInputObject) not hasInput\n")
	// }

	if (input.Source == nil) || (!hasInput) {
		//fmt.Println("(GetStepInputObject) input.Source == nil || (!hasInput)\n")

		if input.Default == nil && input.ValueFrom == "" {

			// cannot detect optional WorkflowStep, thus just return Null
			workunitInputMap[cmdID] = cwl.NewNull()
			return
			//fmt.Println("XXX input:")
			//spew.Dump(input)

			//err = fmt.Errorf("(GetStepInputObject) B) sorry, source, Default and ValueFrom are missing") // TODO StepInputExpressionRequirement
			//return
		}

		if input.Default != nil {
			var defaultValue cwl.CWLType
			defaultValue, err = cwl.NewCWLType(cmdID, "", input.Default, context)
			if err != nil {
				err = fmt.Errorf("(GetStepInputObject) NewCWLTypeFromInterface(input.Default) returns: %s", err.Error())
				return
			}

			if defaultValue == nil {
				err = fmt.Errorf("(GetStepInputObject) default_value == nil ")
				return
			}

			workunitInputMap[cmdID] = defaultValue
		}
	} else {
		fmt.Printf("(GetStepInputObject) not using default\n")
	}
	// TODO

	return
}

// GetStepInputObjects _ Get inputs for workflowStep
func (qm *ServerMgr) GetStepInputObjects(job *Job, workflowInstance *WorkflowInstance, workflowInputMap map[string]cwl.CWLType, workflowStepInputs []*cwl.WorkflowStepInput, context *cwl.WorkflowContext, caller string) (workunitInputMap cwl.JobDocMap, ok bool, reason string, err error) {

	workunitInputMap = make(map[string]cwl.CWLType) // also used for json
	reason = "undefined"

	fmt.Println("(GetStepInputObjects) workflowStepInputs:")
	spew.Dump(workflowStepInputs)

	if workflowStepInputs == nil {
		// empty inputs are ok
		ok = true
		//err = fmt.Errorf("(GetStepInputObjects) workflow_step.In == nil (%s)", workflow_step.Id)
		return
	}

	if len(workflowStepInputs) == 0 {
		// empty inputs are ok
		ok = true
		//err = fmt.Errorf("(GetStepInputObjects) len(workflow_step.In) == 0")
		return
	}

	// 1. find all object source and Default
	// 2. make a map copy to be used in javascript, as "inputs"
	// INPUT_LOOP1
	for inputI, input := range workflowStepInputs {
		// input is a WorkflowStepInput

		ok, reason, err = qm.GetStepInputObject(job, workflowInstance, workflowInputMap, workunitInputMap, inputI, input, context)
		if err != nil {
			err = fmt.Errorf("(GetStepInputObjects) GetStepInputObject returned: %s", err.Error())
			return
		}

		if !ok {
			// input not found but is mibght be optional, simply continue
			ok = true
			//return
		}
	} // end of INPUT_LOOP1
	//fmt.Printf("(GetStepInputObjects) workunit_input_map after first round: \n")
	//spew.Dump(workunitInputMap)

	// 3. evaluate each ValueFrom field, update results
VALUE_FROM_LOOP:
	for _, input := range workflowStepInputs {
		if input.ValueFrom == "" {
			continue VALUE_FROM_LOOP
		}

		id := input.ID
		cmdID := path.Base(id)

		// from CWL doc: The self value of in the parameter reference or expression must be the value of the parameter(s) specified in the source field, or null if there is no source field.

		// #### Create VM ####
		vm := otto.New()

		// set "inputs"

		//func ToValue(value interface{}) (Value, error)

		//var inputs_value otto.Value
		//inputs_value, err = vm.ToValue(workunit_input_map)
		//if err != nil {
		//	return
		//}

		//fmt.Println("(GetStepInputObjects) workunit_input_map:")
		//spew.Dump(workunit_input_map)

		var inputs_json []byte
		inputs_json, err = json.Marshal(workunitInputMap)
		if err != nil {
			err = fmt.Errorf("(GetStepInputObjects) json.Marshal returns: %s", err.Error())
			return
		}
		logger.Debug(3, "SET inputs=%s\n", inputs_json)

		//err = vm.Set("inputs", workunit_input_map)
		//err = vm.Set("inputs_str", inputs_json)
		//if err != nil {
		//	err = fmt.Errorf("(GetStepInputObjects) vm.Set inputs returns: %s", err.Error())
		//	return
		//}

		var js_self cwl.CWLType
		js_self, ok = workunitInputMap[cmdID]
		if !ok {
			//err = fmt.Errorf("(GetStepInputObjects) workunit_input %s not found", cmd_id)
			//return
			logger.Warning("(GetStepInputObjects) workunit_input %s not found", cmdID)
			js_self = cwl.NewNull()
		}

		// TODO check for scatter
		// https://www.commonwl.org/v1.0/Workflow.html#WorkflowStepInput

		if js_self == nil {
			err = fmt.Errorf("(GetStepInputObjects) js_self == nil")
			return
		}

		var self_json []byte
		self_json, err = json.Marshal(js_self)
		if err != nil {
			err = fmt.Errorf("(GetStepInputObjects) json.Marshal returned: %s", err.Error())
			return
		}

		logger.Debug(3, "SET self=%s\n", self_json)

		//err = vm.Set("self", js_self)
		//err = vm.Set("self_str", self_json)
		//if err != nil {
		//	err = fmt.Errorf("(GetStepInputObjects) vm.Set self returns: %s", err.Error())
		//	return
		//}

		//fmt.Printf("input.ValueFrom=%s\n", input.ValueFrom)

		// evaluate $(...) ECMAScript expression
		reg := regexp.MustCompile(`\$\(.+\)`)
		// CWL documentation: http://www.commonwl.org/v1.0/Workflow.html#Expressions

		parsed_str := input.ValueFrom.String()
		//for {

		matches := reg.FindAll([]byte(parsed_str), -1)
		fmt.Printf("()Matches: %d\n", len(matches))
		if len(matches) > 0 {

			concatenate := false
			if len(matches) > 1 {
				concatenate = true
			}

			for _, match := range matches {
				expressionString := bytes.TrimPrefix(match, []byte("$("))
				expressionString = bytes.TrimSuffix(expressionString, []byte(")"))

				javascript_function := fmt.Sprintf("(function(){\n self=%s ; inputs=%s; return %s;\n})()", self_json, inputs_json, expressionString)
				fmt.Printf("%s\n", javascript_function)

				value, xerr := vm.Run(javascript_function)
				if xerr != nil {
					err = fmt.Errorf("(GetStepInputObjects) Javascript complained: A) %s", xerr.Error())
					return
				}
				fmt.Println(reflect.TypeOf(value))

				//if value.IsNumber()
				if concatenate {
					valueStr, xerr := value.ToString()
					if xerr != nil {
						err = fmt.Errorf("(GetStepInputObjects) Cannot convert value to string: %s", xerr.Error())
						return
					}
					parsed_str = strings.Replace(parsed_str, string(match), valueStr, 1)
				} else {

					var value_returned cwl.CWLType
					var exported_value interface{}
					//https://godoc.org/github.com/robertkrimen/otto#Value.Export
					exported_value, err = value.Export()

					if err != nil {
						err = fmt.Errorf("(GetStepInputObjects)  value.Export() returned: %s", err.Error())
						return
					}
					switch exported_value.(type) {

					case string:
						value_returned = cwl.NewString(exported_value.(string))

					case bool:

						value_returned = cwl.NewBooleanFrombool(exported_value.(bool))

					case int:
						value_returned, err = cwl.NewInt(exported_value.(int), context)
						if err != nil {
							err = fmt.Errorf("(NewCWLType) NewInt: %s", err.Error())
							return
						}
					case float32:
						value_returned = cwl.NewFloat(exported_value.(float32))
					case float64:

						exported_valueFloat := exported_value.(float64)
						if math.IsNaN(exported_valueFloat) {
							err = fmt.Errorf("(EvaluateExpression) float64 IsNaN ")
							return
						}

						value_returned = cwl.NewDouble(exported_valueFloat)
					case uint64:
						value_returned, err = cwl.NewInt(exported_value.(int), context)
						if err != nil {
							err = fmt.Errorf("(NewCWLType) NewInt: %s", err.Error())
							return
						}

					case []interface{}: //Array
						err = fmt.Errorf("(GetStepInputObjects) array not supported yet")
						return
					case interface{}: //Object

						value_returned, err = cwl.NewCWLType("", "", exported_value, context)
						if err != nil {
							//fmt.Println("record:")
							//spew.Dump(exported_value)
							err = fmt.Errorf("(GetStepInputObjects) interface{}, NewCWLType returned: %s", err.Error())
							return
						}

					case nil:
						value_returned = cwl.NewNull()
					default:
						err = fmt.Errorf("(GetStepInputObjects) js return type not supoported: (%s)", reflect.TypeOf(exported_value))
						return
					}

					//fmt.Println("value_returned:")
					//spew.Dump(value_returned)
					workunitInputMap[cmdID] = value_returned
					continue VALUE_FROM_LOOP
				}
			} // for matches

			//if concatenate
			workunitInputMap[cmdID] = cwl.NewString(parsed_str)

			continue VALUE_FROM_LOOP
		} // if matches
		//}

		//fmt.Printf("parsed_str: %s\n", parsed_str)

		// evaluate ${...} ECMAScript function body
		reg = regexp.MustCompile(`(?s)\${.+}`) // s-flag is needed to include newlines

		// CWL documentation: http://www.commonwl.org/v1.0/Workflow.html#Expressions

		matches = reg.FindAll([]byte(parsed_str), -1)
		//fmt.Printf("{}Matches: %d\n", len(matches))
		if len(matches) == 0 {
			workunitInputMap[cmdID] = cwl.NewString(parsed_str)
			continue VALUE_FROM_LOOP
		}

		if len(matches) == 1 {
			match := matches[0]
			expressionString := bytes.TrimPrefix(match, []byte("${"))
			expressionString = bytes.TrimSuffix(expressionString, []byte("}"))

			javascriptFunction := fmt.Sprintf("(function(){\n self=%s ; inputs=%s; %s \n})()", self_json, inputs_json, expressionString)
			fmt.Printf("%s\n", javascriptFunction)

			value, xerr := vm.Run(javascriptFunction)
			if xerr != nil {
				err = fmt.Errorf("Javascript complained: B) %s", xerr.Error())
				return
			}

			value_exported, _ := value.Export()

			fmt.Printf("reflect.TypeOf(value_exported): %s\n", reflect.TypeOf(value_exported))

			var valueCwl cwl.CWLType
			valueCwl, err = cwl.NewCWLType("", "", value_exported, context)
			if err != nil {
				err = fmt.Errorf("(NewWorkunit) Error parsing javascript VM result value, cwl.NewCWLType returns: %s", err.Error())
				return
			}

			workunitInputMap[cmdID] = valueCwl
			continue VALUE_FROM_LOOP
		}

		err = fmt.Errorf("(NewWorkunit) ValueFrom contains more than one ECMAScript function body")
		return

	} // end of VALUE_FROM_LOOP

	//fmt.Println("(GetStepInputObjects) workunit_input_map after ValueFrom round:")
	//spew.Dump(workunit_input_map)

	for key, value := range workunitInputMap {
		fmt.Printf("workunit_input_map: %s -> %s (%s)\n", key, value.String(), reflect.TypeOf(value))

	}
	ok = true
	return
}

// CreateAndEnqueueWorkunits _
func (qm *ServerMgr) CreateAndEnqueueWorkunits(task *Task, job *Job) (count int, err error) {
	//logger.Debug(3, "(CreateAndEnqueueWorkunits) starting")
	//fmt.Println("--CreateAndEnqueueWorkunits--")
	//spew.Dump(task)
	var workunits []*Workunit
	workunits, err = task.CreateWorkunits(qm, job)
	if err != nil {
		err = fmt.Errorf("(CreateAndEnqueueWorkunits) task.CreateWorkunits returned: %s", err.Error())
		return
	}
	for _, wu := range workunits {
		err = qm.workQueue.Add(wu)
		if err != nil {
			err = fmt.Errorf("(CreateAndEnqueueWorkunits) qm.workQueue.Add returned: %s", err.Error())
			return
		}
		id := wu.GetID()
		err = qm.CreateWorkPerf(id)
		if err != nil {
			err = fmt.Errorf("(CreateAndEnqueueWorkunits) qm.CreateWorkPerf returned: %s", err.Error())
			return
		}
	}
	count = len(workunits)
	return
}

func (qm *ServerMgr) createOutputNode(task *Task) (err error) {
	err = task.LockNamed("createOutputNode")
	if err != nil {
		return
	}
	defer task.Unlock()

	var modified bool
	for _, io := range task.Outputs {
		if io.Type == "update" {
			// this an update output, it will update an existing shock node and not create a new one (it will update metadata of the shock node)
			if (io.Node == "") || (io.Node == "-") {
				if io.Origin == "" {
					// it may be in inputs
					for _, input := range task.Inputs {
						if io.FileName == input.FileName {
							io.Node = input.Node
							io.Size = input.Size
							io.Url = input.Url
							modified = true
						}
					}
					if (io.Node == "") || (io.Node == "-") {
						// still missing
						err = fmt.Errorf("update output %s in task %s is missing required origin", io.FileName, task.ID)
						return
					}
				} else {
					// find predecessor task
					var preId Task_Unique_Identifier
					preId, err = New_Task_Unique_Identifier(task.JobId, io.Origin)
					if err != nil {
						err = fmt.Errorf("New_Task_Unique_Identifier returned: %s", err.Error())
						return
					}
					var preTaskStr string
					preTaskStr, err = preId.String()
					if err != nil {
						err = fmt.Errorf("task.String returned: %s", err.Error())
						return
					}
					preTask, ok, xerr := qm.TaskMap.Get(preId, true)
					if xerr != nil {
						err = fmt.Errorf("predecessor task %s not found for task %s: %s", preTaskStr, task.ID, xerr.Error())
						return
					}
					if !ok {
						err = fmt.Errorf("predecessor task %s not found for task %s", preTaskStr, task.ID)
						return
					}

					// find predecessor output
					preTaskIO, xerr := preTask.GetOutput(io.FileName)
					if xerr != nil {
						err = fmt.Errorf("unable to get IO for predecessor task %s, file %s: %s", preTask.ID, io.FileName, err.Error())
						return
					}

					// copy if not already done
					if io.Node != preTaskIO.Node {
						io.Node = preTaskIO.Node
						modified = true
					}
					if io.Size != preTaskIO.Size {
						io.Size = preTaskIO.Size
						modified = true
					}
					if io.Url != preTaskIO.Url {
						io.Url = preTaskIO.Url
						modified = true
					}
				}
				logger.Debug(2, "(createOutputNode) outout %s in task %s is an update of node %s", io.FileName, task.ID, io.Node)
			}
		} else {
			// POST empty shock node for this output
			logger.Debug(2, "(createOutputNode) posting output Shock node for file %s in task %s", io.FileName, task.ID)

			sc := shock.ShockClient{Host: io.Host, Token: task.Info.DataToken}
			var nodeid string
			nodeid, err = sc.CreateNode(io.FileName, task.TotalWork)
			if err != nil {
				return
			}
			io.Node = nodeid
			_, err = io.DataUrl()
			if err != nil {
				return
			}
			modified = true
			logger.Debug(2, "(createOutputNode) task %s: output Shock node created, node=%s", task.ID, nodeid)
		}
	}

	if modified {
		err = dbUpdateJobTaskIO(task.JobId, task.WorkflowInstanceID, task.ID, "outputs", task.Outputs)
		if err != nil {
			err = fmt.Errorf("unable to save task outputs to mongodb, task=%s: %s", task.ID, err.Error())
		}
	}
	return
}

//---end of task methods---

// taskCompletedScatter is called for every scatter child, but only the last child performs collection of results
func (qm *ServerMgr) taskCompletedScatter(job *Job, wi *WorkflowInstance, task *Task) (err error) {

	var taskStr string
	taskStr, err = task.String()
	if err != nil {
		err = fmt.Errorf("(taskCompletedScatter) task.String returned: %s", err.Error())
		return
	}

	logger.Debug(3, "(taskCompletedScatter) %s Scatter_parent exists", taskStr)
	scatterParentID := *task.ScatterParent
	var scatterParentTask *Task
	var ok bool
	scatterParentTask, ok, err = qm.TaskMap.Get(scatterParentID, true)
	if err != nil {
		err = fmt.Errorf("(taskCompletedScatter) qm.TaskMap.Get returned: %s", err.Error())
		return
	}
	if !ok {
		err = fmt.Errorf("(taskCompletedScatter) Scatter_Parent task %s not found", scatterParentID)
		return
	}

	// (taskCompletedScatter) get scatter sibblings to see if they are done
	var children []*Task
	children, err = scatterParentTask.GetScatterChildren(wi, qm)
	if err != nil {

		length, _ := qm.TaskMap.Len()

		var tasks []*Task
		tasks, _ = qm.TaskMap.GetTasks()
		for _, task := range tasks {
			fmt.Printf("(taskCompletedScatter) got task %s\n", task.ID)
		}

		err = fmt.Errorf("(taskCompletedScatter) (scatter) GetScatterChildren returned: %s (total: %d)", err.Error(), length)
		return
	}

	//fmt.Printf("XXX children: %d\n", len(children))

	scatterComplete := true
	for _, childTask := range children {

		var childState string
		childState, err = childTask.GetState()
		if err != nil {
			err = fmt.Errorf("(taskCompletedScatter) child_task.GetState returned: %s", err.Error())
			return
		}
		logger.Debug(3, "(taskCompletedScatter) childState: %s (%s)", childState, childTask.ID)
		if childState != TASK_STAT_COMPLETED {
			scatterComplete = false
			break
		}
	}

	if !scatterComplete {
		logger.Debug(3, "(taskCompletedScatter) scatter not complete ")
		// nothing to do here, scatter is not complete
		return
	}

	logger.Debug(3, "(taskCompletedScatter) scatter complete, try to finalize... ")

	ok, err = scatterParentTask.Finalize() // make sure this is the last scatter task
	if err != nil {
		err = fmt.Errorf("(taskCompletedScatter) scatter_parent_task.Finalize returned: %s", err.Error())
		return
	}

	if !ok {
		logger.Debug(3, "(taskCompletedScatter) somebody else is finalizing")
		// somebody else is finalizing
		return
	}

	// ***************************************
	//           scatter_complete
	// ***************************************

	logger.Debug(3, "(taskCompletedScatter) scatter complete, finalizing...")

	scatterParentStep := scatterParentTask.WorkflowStep

	scatterParentTask.StepOutput = &cwl.Job_document{}

	//scontext := job.WorkflowContext

	//fmt.Printf("XXX start\n")
	for i := range scatterParentStep.Out {
		//fmt.Printf("XXX loop %d\n", i)
		workflowStepOutput := scatterParentStep.Out[i]
		workflowStepOutputID := workflowStepOutput.Id

		workflowStepOutputIDBase := path.Base(workflowStepOutputID)

		outputArray := cwl.Array{}

		for _, childTask := range children {
			//fmt.Printf("XXX inner loop %d\n", i)

			if childTask.StepOutput == nil {
				err = fmt.Errorf("(taskCompletedScatter) childTask.StepOutput == nil")
				return
			}
			jobDoc := childTask.StepOutput
			var childOutput cwl.CWLType
			childOutput, ok = jobDoc.Get(workflowStepOutputIDBase)
			if !ok {
				//fmt.Printf("XXX job_doc.Get failed\n")
				err = fmt.Errorf("(taskCompletedScatter) job_doc.Get failed: %s ", err.Error())
				return
			}
			//fmt.Println("child_output:")
			//spew.Dump(child_output)
			outputArray = append(outputArray, childOutput)
			//fmt.Println("output_array:")
			//spew.Dump(output_array)

		}
		// err = context.Add(workflowStepOutputID, &outputArray, "taskCompletedScatter")
		// if err != nil {
		// 	err = fmt.Errorf("(taskCompletedScatter) context.Add returned: %s", err.Error())
		// 	return
		// }
		//fmt.Println("final output_array:")
		//spew.Dump(output_array)
		scatterParentTask.StepOutput = scatterParentTask.StepOutput.Add(workflowStepOutputID, &outputArray)

	}

	task = scatterParentTask

	///wi_local_id := task.WorkflowInstanceID
	//var wi *WorkflowInstance
	//if wi_local_id != "" {
	//	wi, ok, err = task.GetWorkflowInstance()

	//}
	// err = task.SetState(wi, TASK_STAT_COMPLETED, true)
	// if err != nil {
	// 	err = fmt.Errorf("(taskCompletedScatter) task.SetState returned: %s", err.Error())
	// 	return
	// }

	//log event about task done (TD)
	err = qm.FinalizeTaskPerf(task)
	if err != nil {
		err = fmt.Errorf("(taskCompletedScatter) FinalizeTaskPerf returned: %s", err.Error())
		return
	}
	logger.Event(event.TASK_DONE, "taskID="+taskStr)

	//update the info of the job which the task is belong to, could result in deletion of the
	//task in the task map when the task is the final task of the job to be done.
	err = qm.taskCompleted(wi, task) //task state QUEUED -> COMPLETED
	if err != nil {
		err = fmt.Errorf("(taskCompletedScatter) updateJobTask returned: %s", err.Error())
		return
	}
	logger.Debug(3, "(taskCompletedScatter) finished")
	return
}

// completeSubworkflow checks if all steps have completed (should not be required if counters are used)
// invoked by qm.WISetState or
func (qm *ServerMgr) completeSubworkflow(job *Job, workflowInstance *WorkflowInstance) (ok bool, reason string, err error) {

	var wfl *cwl.Workflow

	// if this is a subworkflow, check if sibblings are complete
	//var context *cwl.WorkflowContext

	var wiState string
	wiState, err = workflowInstance.GetState(true)
	if err != nil {
		err = fmt.Errorf("(completeSubworkflow) workflowInstance.GetState returned: %s", err.Error())
		return
	}

	if wiState == WIStateCompleted {
		ok = true
		return
	}

	context := job.WorkflowContext

	workflowInstanceID, _ := workflowInstance.GetID(true)
	logger.Debug(3, "(completeSubworkflow) start: %s", workflowInstanceID)
	// check tasks
	for _, task := range workflowInstance.Tasks {

		var taskState string
		taskState, _ = task.GetState()

		if taskState != TASK_STAT_COMPLETED {
			ok = false
			reason = "(completeSubworkflow) a task is not completed yet"
			return
		}

	}

	// check subworkflows
	for _, subworkflow := range workflowInstance.Subworkflows {

		var subWI *WorkflowInstance
		subWI, ok, err = job.GetWorkflowInstance(subworkflow, true)
		if err != nil {
			err = fmt.Errorf("(completeSubworkflow) job.GetWorkflowInstance returned: %s", err.Error())
			return
		}
		if !ok {
			reason = fmt.Sprintf("(completeSubworkflow) subworkflow %s not found", subworkflow)
			return
		}

		subWIState, _ := subWI.GetState(true)

		if subWIState != WIStateCompleted {
			ok = false
			reason = fmt.Sprintf("(completeSubworkflow) subworkflow %s is not completed", subworkflow)
			return
		}

	}

	// *************
	// subworkflow complete, now collect outputs !
	// *************

	wfl, err = workflowInstance.GetWorkflow(context)
	if err != nil {
		err = fmt.Errorf("(completeSubworkflow) workflowInstance.GetWorkflow returned: %s", err.Error())
		return
	}

	// reminder: at this point all tasks in subworkflow are complete, see above

	workflowInputs := workflowInstance.Inputs

	workflowInputsMap := workflowInputs.GetMap()

	workflowOutputsMap := make(cwl.JobDocMap)

	// collect sub-workflow outputs, put results in workflow_outputs_map

	logger.Debug(3, "(completeSubworkflow) %s len(wfl.Outputs): %d", workflowInstanceID, len(wfl.Outputs))

	for _, output := range wfl.Outputs { // WorkflowOutputParameter http://www.commonwl.org/v1.0/Workflow.html#WorkflowOutputParameter

		outputID := output.Id

		if output.OutputBinding != nil {
			// see http://www.commonwl.org/v1.0/Workflow.html#CommandOutputBinding
			//spew.Dump(output.OutputBinding)
			// import path
			// use https://golang.org/pkg/path/#Match
			// iterate over output files
			//for _, value := range workflow_inputs_map {
			//fmt.Println("key: " + key)

			//	_, ok := value.(*cwl.File)
			//	if !ok {
			//		continue
			//	}

			//fmt.Println("base: " + file.Basename)

			//}

			//panic("ok")
			err = fmt.Errorf("(completeSubworkflow) Workflow output outputbinding not supported yet")
			return
		}

		var expectedTypesRaw []interface{}

		switch output.Type.(type) {
		case []interface{}:
			expectedTypesRaw = output.Type.([]interface{})
		case []cwl.CWLType_Type:

			expectedTypesRawArray := output.Type.([]cwl.CWLType_Type)
			for i, _ := range expectedTypesRawArray {
				expectedTypesRaw = append(expectedTypesRaw, expectedTypesRawArray[i])

			}

		default:
			expectedTypesRaw = append(expectedTypesRaw, output.Type)
			//expected_types_raw = []interface{output.Type}
		}
		expectedTypes := []cwl.CWLType_Type{}

		isOptional := false

		var schemata []cwl.CWLType_Type
		schemata, err = job.WorkflowContext.GetSchemata()
		if err != nil {
			err = fmt.Errorf("(completeSubworkflow) job.CWL_collection.GetSchemata returned: %s", err.Error())
			return
		}

		for _, rawType := range expectedTypesRaw {
			var typeCorrectArray []cwl.CWLType_Type
			typeCorrectArray, err = cwl.NewCWLType_Type(schemata, rawType, "Output", context)
			if err != nil {
				//spew.Dump(expected_types_raw)
				//fmt.Println("---")
				//spew.Dump(raw_type)

				err = fmt.Errorf("(completeSubworkflow) could not convert element of output.Type into cwl.CWLType_Type: %s", err.Error())
				//fmt.Printf(err.Error())
				//panic("raw_type problem")
				return
			}
			for _, correctType := range typeCorrectArray {
				expectedTypes = append(expectedTypes, correctType)
				if correctType == cwl.CWLNull {
					isOptional = true
				}
			}

		}

		// search the outputs and stick them in workflow_outputs_map

		outputSource := output.OutputSource

		switch outputSource.(type) {
		case string:
			outputSourceString := outputSource.(string)
			// example: "#preprocess-fastq.workflow.cwl/rejected2fasta/file"

			var obj cwl.CWLType
			//var ok bool
			//var reason string
			obj, ok, reason, err = qm.getCWLSource(job, workflowInstance, workflowInputsMap, outputSourceString, true, job.WorkflowContext)
			if err != nil {
				err = fmt.Errorf("(completeSubworkflow) A) getCWLSource returns: %s", err.Error())
				return
			}
			skip := false
			if !ok {
				if isOptional {
					skip = true
				} else {
					err = fmt.Errorf("(completeSubworkflow) A) source %s not found by getCWLSource (getCWLSource reason: %s)", outputSourceString, reason)
					return
				}
			}

			if !skip {
				hasType, xerr := cwl.TypeIsCorrect(expectedTypes, obj, context)
				if xerr != nil {
					err = fmt.Errorf("(completeSubworkflow) TypeIsCorrect: %s", xerr.Error())
					return
				}
				if !hasType {
					err = fmt.Errorf("(completeSubworkflow) A) workflow_ouput %s (type: %s), does not match expected types %s", outputID, reflect.TypeOf(obj), expectedTypes)
					return
				}

				workflowOutputsMap[outputID] = obj
			}
		case []string:
			outputSourceArrayOfString := outputSource.([]string)

			if len(outputSourceArrayOfString) == 0 {
				if !isOptional {
					err = fmt.Errorf("(completeSubworkflow) output_source array (%s) is empty, but a required output", outputID)
					return
				}
			}

			output_array := cwl.Array{}

			for _, outputSourceString := range outputSourceArrayOfString {
				var obj cwl.CWLType
				//var ok bool
				obj, ok, _, err = qm.getCWLSource(job, workflowInstance, workflowInputsMap, outputSourceString, true, job.WorkflowContext)
				if err != nil {
					err = fmt.Errorf("(completeSubworkflow) B) (%s) getCWLSource returns: %s", workflowInstanceID, err.Error())
					return
				}

				skip := false
				if !ok {

					if isOptional {
						skip = true
					} else {

						err = fmt.Errorf("(completeSubworkflow) B) (%s) source %s not found", workflowInstanceID, outputSourceString)
						return
					}
				}

				if !skip {
					has_type, xerr := cwl.TypeIsCorrect(expectedTypes, obj, context)
					if xerr != nil {
						err = fmt.Errorf("(completeSubworkflow) TypeIsCorrect: %s", xerr.Error())
						return
					}
					if !has_type {
						err = fmt.Errorf("(completeSubworkflow) B) workflow_ouput %s, does not match expected types %s", outputID, expectedTypes)
						return
					}
					//fmt.Println("obj:")
					//spew.Dump(obj)
					output_array = append(output_array, obj)
				}
			}

			if len(output_array) > 0 {
				workflowOutputsMap[outputID] = &output_array
			} else {
				if !isOptional {
					err = fmt.Errorf("(completeSubworkflow) array with output_id %s is empty, but a required output", outputID)
					return
				}
			}
			//fmt.Println("workflow_outputs_map:")
			//spew.Dump(workflow_outputs_map)

		default:
			err = fmt.Errorf("(completeSubworkflow) output.OutputSource has to be string or []string, but I got type %s", spew.Sdump(outputSource))
			return

		}

	}

	var workflowOutputsArray cwl.Job_document
	workflowOutputsArray, err = workflowOutputsMap.GetArray()
	if err != nil {
		err = fmt.Errorf("(completeSubworkflow) workflow_outputs_map.GetArray returned: %s", err.Error())
		return
	}

	logger.Debug(3, "(completeSubworkflow) %s len(workflowOutputsArray): %d", workflowInstanceID, len(workflowOutputsArray))

	err = workflowInstance.SetOutputs(workflowOutputsArray, context, true)
	if err != nil {
		err = fmt.Errorf("(completeSubworkflow) workflowInstance.SetOutputs returned: %s", err.Error())
		return
	}

	// stick outputs in context, using correct Step-name (depends on if it is a embedded workflow)
	logger.Debug(3, "(completeSubworkflow) workflowInstance.Outputs: %d", len(workflowInstance.Outputs))
	//for output := range workflowInstance.Outputs {
	//	logger.Debug(3, "(completeSubworkflow) iteration %d", output)
	//outputNamed := &workflowInstance.Outputs[output]
	//outputNamedBase := path.Base(outputNamed.Id)

	//	prefix := path.Dir(outputNamed.Id)

	//logger.Debug(3, "(completeSubworkflow) old name: %s", outputNamed.Id)

	//prefixBase := path.Base(prefix)
	//newName := outputNamed.Id
	//if len(prefixBase) == 36 { // TODO: find better way of detecting embedded workflow
	// case: emebedded workflow
	//	prefix = path.Dir(prefix)
	//	newName = prefix + "/" + outputNamedBase
	//}

	//fmt.Printf("new name: %s\n", new_name)
	//logger.Debug(3, "(completeSubworkflow) new name: %s", newName)
	//err = context.Add(newName, outputNamed.Value, "completeSubworkflow")
	//if err != nil {
	//	err = fmt.Errorf("(completeSubworkflow) context.Add returned: %s", err.Error())
	//	return
	//}

	//}

	if workflowInstance.RemainSteps > 0 {
		err = fmt.Errorf("(completeSubworkflow) RemainSteps > 0 cannot complete")
		return
	}

	err = workflowInstance.SetState(WIStateCompleted, true, "completeSubworkflow")
	if err != nil {
		err = fmt.Errorf("(completeSubworkflow) workflowInstance.SetState returned: %s", err.Error())
		return
	}

	// logger.Debug(3, "(completeSubworkflow) job.WorkflowInstancesRemain: (before) %d", job.WorkflowInstancesRemain)
	// err = job.IncrementWorkflowInstancesRemain(-1, DbSyncTrue, true)
	// if err != nil {
	// 	err = fmt.Errorf("(completeSubworkflow) job.IncrementWorkflowInstancesRemain returned: %s", err.Error())
	// 	return
	// }

	// ***** notify parent workflow or job (this workflow might have been the last step)

	workflowInstanceLocalID := workflowInstance.LocalID
	logger.Debug(3, "(completeSubworkflow) completes with workflowInstanceLocalID: %s", workflowInstanceLocalID)

	if workflowInstanceLocalID == job.Entrypoint {
		// last workflowInstance -> notify job

		// this was the main workflow, all done!

		err = qm.finalizeJob(job)
		if err != nil {
			err = fmt.Errorf("(completeSubworkflow) qm.finalizeJob returned: %s", err.Error())
			return
		}

		return
	}

	// check if parent needs to be notified
	// update Remain varable and complete if necessary
	logger.Debug(3, "(completeSubworkflow) check parent")
	var parent *WorkflowInstance
	parent, err = workflowInstance.GetParent(true)
	if err != nil {
		err = fmt.Errorf("(completeSubworkflow) workflowInstance.GetParent returned: %s", err.Error())
		return
	}

	// notify parent
	var parentRemain int
	parentRemain, err = parent.IncrementRemainSteps(-1, true)
	if err != nil {
		err = fmt.Errorf("(completeSubworkflow) parent.IncrementRemainSteps returned: %s", err.Error())
		return
	}

	if parentRemain > 0 {
		// no need to notify
		logger.Debug(3, "(completeSubworkflow) no need to complete parent workflow")
		return
	}

	logger.Debug(3, "(completeSubworkflow) complete parent workflow")

	// complete parent workflow
	var parentResult string
	ok, parentResult, err = qm.completeSubworkflow(job, parent) // recursive call
	if err != nil {
		err = fmt.Errorf("(completeSubworkflow) recursive call of qm.completeSubworkflow returned: %s", err.Error())
		return
	}
	if !ok {
		err = fmt.Errorf("(completeSubworkflow) qm.completeSubworkflow could not complete, reason: %s", parentResult)
		return
	}

	logger.Debug(3, "(completeSubworkflow) done")

	return
}

// invoked when tasks completes
// update job info
// update parent task of a scatter task
func (qm *ServerMgr) taskCompleted(wi *WorkflowInstance, task *Task) (err error) {

	// TODO I am not sure why this function is invoked for every state change....

	var taskState string
	taskState, err = task.GetState()
	if err != nil {
		err = fmt.Errorf("(taskCompleted) task.GetState() returned: %s", err.Error())
		return
	}

	if taskState == TASK_STAT_COMPLETED {
		err = fmt.Errorf("(taskCompleted) task state is alreay TASK_STAT_COMPLETED")
		return
	}

	var job *Job
	job, err = task.GetJob(time.Second*90, true)
	if err != nil {
		err = fmt.Errorf("(taskCompleted) task.GetJob returned: %s", err.Error())
		return
	}

	var taskStr string
	taskStr, err = task.String()
	if err != nil {
		err = fmt.Errorf("(taskCompleted) task.String returned: %s", err.Error())
		return
	}

	// check if this was the last task in a subworkflow

	// check if this task has a parent

	if task.WorkflowStep == nil {
		logger.Debug(3, "(taskCompleted) task.WorkflowStep == nil ")
	} else {
		logger.Debug(3, "(taskCompleted) task.WorkflowStep != nil ")
	}

	logger.Debug(3, "(taskCompleted) task.WorkflowStep != nil (%s)", taskStr)

	err = task.SetState(TASK_STAT_COMPLETED, true, "taskCompleted")
	if err != nil {
		err = fmt.Errorf("(taskCompleted) task.SetState returned: %s", err.Error())
		return
	}

	if task.ScatterParent != nil {

		// var wi *WorkflowInstance
		// var ok bool
		// wi, ok, err = task.GetWorkflowInstance()
		// if err != nil {
		// 	err = fmt.Errorf("(taskCompleted) task.GetWorkflowInstance returned: %s", err.Error())
		// 	return
		// }

		// if !ok {
		// 	err = fmt.Errorf("(taskCompleted) task has no WorkflowInstance ?! ")
		// 	return
		// }

		// process scatter children
		err = qm.taskCompletedScatter(job, wi, task)
		if err != nil {
			err = fmt.Errorf("(taskCompleted) taskCompletedScatter returned: %s", err.Error())
			return
		}

	} else { // end task.Scatter_parent != nil
		logger.Debug(3, "(taskCompleted) %s  No Scatter_parent", taskStr)
	}

	// err = task.SetState(TASK_STAT_COMPLETED, true, "taskCompleted")
	// if err != nil {
	// 	err = fmt.Errorf("(taskCompleted) task.SetState returned: %s", err.Error())
	// 	return
	// }

	_ = task.SetTaskNotReadyReason("", true)

	// ******************
	// check if workflowInstance needs to be completed

	// workflowInstanceID := task.WorkflowInstanceID

	// var workflowInstance *WorkflowInstance
	// var ok bool
	// workflowInstance, ok, err = job.GetWorkflowInstance(workflowInstanceID, true)
	// if err != nil {
	// 	err = fmt.Errorf("(taskCompleted) job.GetWorkflowInstance returned: %s", err.Error())
	// 	return
	// }

	// if !ok {
	// 	err = fmt.Errorf("(taskCompleted) WorkflowInstance %s not found", err.Error())
	// 	return
	// }

	if wi != nil {

		//logger.Debug(3, "(taskCompleted) TASK_STAT_COMPLETED  / remaining steps for subworkflow %s: %d", taskStr, subworkflowRemainSteps)

		var subworkflowRemainSteps int
		subworkflowRemainSteps, err = wi.GetRemainSteps(true)
		if err != nil {
			err = fmt.Errorf("(taskCompleted) workflowInstance.GetRemainSteps returned: %s", err.Error())
			return
		}

		//subworkflow_remain_tasks, err = workflowInstance.DecreaseRemainSteps()
		//if err != nil {
		//	err = fmt.Errorf("(taskCompleted) workflowInstance.DecreaseRemainSteps returned: %s", err.Error())
		//	return

		//}

		logger.Debug(3, "(taskCompleted) TASK_STAT_COMPLETED  / remaining steps for subworkflow %s: %d", taskStr, subworkflowRemainSteps)

		//logger.Debug(3, "(taskCompleted) workflowInstance %s remaining tasks: %d (total %d or %d)", workflowInstanceID, subworkflowRemainSteps, workflowInstance.TaskCount(), workflowInstance.TotalTasks)

		if subworkflowRemainSteps > 0 {
			return
		}

		// ******************
		// This is the last task/subworkflow.

		//TODO find a way to lock this

		// subworkflow completed.
		var reason string
		var ok bool
		ok, reason, err = qm.completeSubworkflow(job, wi) // taskCompleted
		if err != nil {
			err = fmt.Errorf("(taskCompleted) completeSubworkflow returned: %s", err.Error())
			return
		}
		if !ok {
			err = fmt.Errorf("(taskCompleted) completeSubworkflow not ok, reason: %s", reason)
			return
		}

		return
	}

	// old-style AWE job only

	err = job.IncrementRemainTasks(-1)
	if err != nil {
		err = fmt.Errorf("(taskCompleted) IncrementRemainTasks returned: %s", err.Error())
		return
	}

	jobRemainTasks := job.RemainTasks

	if jobRemainTasks > 0 { //#####################################################################
		return
	}

	err = qm.finalizeJob(job)
	if err != nil {
		err = fmt.Errorf("(taskCompleted) qm.finalizeJob returned: %s", err.Error())
		return
	}

	// this is a recursive call !
	// if parent_task_to_complete != nil {

	// 	if parent_task_to_complete.State != TASK_STAT_COMPLETED {
	// 		err = fmt.Errorf("(taskCompleted) qm.taskCompleted(parent_task_to_complete) parent_task_to_complete.State != TASK_STAT_COMPLETED")
	// 		return
	// 	}
	// 	err = qm.taskCompleted(parent_task_to_complete)
	// 	if err != nil {
	// 		err = fmt.Errorf("(taskCompleted) qm.taskCompleted(parent_task_to_complete) returned: %s", err.Error())
	// 		return
	// 	}
	// }

	return
}

func (qm *ServerMgr) finalizeJob(job *Job) (err error) {

	jobState, err := job.GetState(true)
	if err != nil {
		err = fmt.Errorf("(updateJobTask) job.GetState returned: %s", err.Error())
		return
	}
	if jobState == JOB_STAT_COMPLETED {
		err = fmt.Errorf("(updateJobTask) job state is already JOB_STAT_COMPLETED")
		return
	}

	err = job.SetState(JOB_STAT_COMPLETED, nil)
	if err != nil {
		err = fmt.Errorf("(updateJobTask) job.SetState returned: %s", err.Error())
		return
	}

	var jobid string
	jobid, err = job.GetID(true)
	if err != nil {
		err = fmt.Errorf("(updateJobTask) job.GetId returned: %s", err.Error())
		return
	}
	qm.FinalizeJobPerf(jobid)
	qm.LogJobPerf(jobid)
	qm.removeActJob(jobid)
	//delete tasks in task map
	//delete from shock output flagged for deletion

	modified := 0
	for i, task := range job.TaskList() {
		// delete nodes that have been flagged to be deleted
		modified += task.DeleteOutput()
		modified += task.DeleteInput()
		//combined_id := jobid + "_" + task.Id

		id, _ := task.GetID("updateJobTask." + strconv.Itoa(i))

		_, _, err = qm.TaskMap.Delete(id)
		if err != nil {
			err = fmt.Errorf("(updateJobTask) qm.TaskMap.Delete returned: %s", err.Error())
			return
		}
	}

	if modified > 0 {
		// save only is something has changed
		job.Save() // TODO avoid this, try partial updates
	}

	//set expiration from conf if not set
	nullTime := time.Time{}
	var job_expiration time.Time
	job_expiration, err = dbGetJobFieldTime(jobid, "expiration")
	if err != nil {
		err = fmt.Errorf("(updateJobTask) dbGetJobFieldTime returned: %s", err.Error())
		return
	}

	if job_expiration == nullTime {
		expire := conf.GLOBAL_EXPIRE

		var job_info_pipeline string
		job_info_pipeline, err = dbGetJobFieldString(jobid, "info.pipeline")
		if err != nil {
			err = fmt.Errorf("(updateJobTask) dbGetJobFieldTime returned: %s", err.Error())
			return
		}

		if val, ok := conf.PIPELINE_EXPIRE_MAP[job_info_pipeline]; ok {
			expire = val
		}
		if expire != "" {
			err = job.SetExpiration(expire)
			if err != nil {
				err = fmt.Errorf("(updateJobTask) SetExpiration returned: %s", err.Error())
				return
			}
		}
	}
	//log event about job done (JD)
	logger.Event(event.JOB_DONE, "jobid="+job.ID+";name="+job.Info.Name+";project="+job.Info.Project+";user="+job.Info.User)

	return
}

// happens when a client checks out a workunit
//update job/task states from "queued" to "in-progress" once the first workunit is checked out
func (qm *ServerMgr) UpdateJobTaskToInProgress(works []*Workunit) (err error) {
	for _, work := range works {
		//job_was_inprogress := false
		//task_was_inprogress := false
		taskid := work.GetTask()
		jobid := work.JobId

		// get job state
		job, xerr := GetJob(jobid)
		if xerr != nil {
			err = xerr
			return
		}

		jobState, xerr := job.GetState(true)
		if xerr != nil {
			err = xerr
			return
		}

		//update job status
		if jobState != JOB_STAT_INPROGRESS {
			err = job.SetState(JOB_STAT_INPROGRESS, nil)
			if err != nil {
				return
			}
			qm.UpdateJobPerfStartTime(jobid)
		}

		task, ok, err := qm.TaskMap.Get(taskid, true)
		if err != nil {
			logger.Error("(UpdateJobTaskToInProgress) %s", err.Error())
			continue
		}
		if !ok {
			logger.Error("(UpdateJobTaskToInProgress) task %s not found", taskid)
			continue
		}

		taskState, err := task.GetState()
		if err != nil {
			logger.Error("(UpdateJobTaskToInProgress) dbGetJobTaskField: %s", err.Error())
			continue
		}

		if taskState != TASK_STAT_INPROGRESS {
			err := task.SetState(TASK_STAT_INPROGRESS, true, "UpdateJobTaskToInProgress")
			if err != nil {
				logger.Error("(UpdateJobTaskToInProgress) could not update task %s", taskid)
				continue
			}
			err = qm.UpdateTaskPerfStartTime(task)
			if err != nil {
				logger.Error("(UpdateJobTaskToInProgress) UpdateTaskPerfStartTime: %s", err.Error())
				continue
			}

		}
	}
	return
}

func (qm *ServerMgr) IsJobRegistered(id string) bool {
	if qm.isActJob(id) {
		return true
	}
	if qm.isSusJob(id) {
		return true
	}
	return false
}

// SuspendJob use for JOB_STAT_SUSPEND and JOB_STAT_FAILED_PERMANENT
// job is optional
func (qm *ServerMgr) SuspendJob(jobid string, job *Job, jerror *JobError) (err error) {

	if job == nil {
		job, err = GetJob(jobid)
		if err != nil {
			return
		}
	}

	err = job.SetState(jerror.Status, nil)
	if err != nil {
		return
	}

	// set error struct
	err = job.SetError(jerror)
	if err != nil {
		return
	}

	//suspend queueing workunits
	var workunit_list []*Workunit
	workunit_list, err = qm.workQueue.GetAll()
	if err != nil {
		return
	}

	new_workState := WORK_STAT_SUSPEND
	new_taskState := TASK_STAT_SUSPEND
	this_event := event.JOB_SUSPEND
	if jerror.Status == JOB_STAT_FAILED_PERMANENT {
		new_workState = WORK_STAT_FAILED_PERMANENT
		new_taskState = TASK_STAT_FAILED_PERMANENT
		this_event = event.JOB_FAILED_PERMANENT
	}

	// update all workunits
	for _, workunit := range workunit_list {
		workid := workunit.Workunit_Unique_Identifier
		parentid := workunit.JobId
		//parentid, _ := GetJobIdByWorkId(workid)
		if jobid == parentid {
			qm.workQueue.StatusChange(workid, nil, new_workState, "see job error")
		}
	}

	//suspend parsed tasks
	for _, task := range job.Tasks {
		var taskState string
		taskState, err = task.GetState()
		if err != nil {
			continue
		}
		if taskState == TASK_STAT_QUEUED || taskState == TASK_STAT_READY || taskState == TASK_STAT_INPROGRESS {
			err = task.SetState(new_taskState, true, "SuspendJob")
			if err != nil {
				logger.Error("(SuspendJob) : %s", err.Error())
				continue
			}
		}
	}
	qm.LogJobPerf(jobid)
	qm.removeActJob(jobid)

	// log event and reason
	var reason string
	if jerror.ServerNotes != "" {
		reason = jerror.ServerNotes
	} else if jerror.WorkNotes != "" {
		reason = jerror.WorkNotes
	}
	logger.Event(this_event, "jobid="+jobid+";reason="+reason)
	return
}

// DeleteJobByUser _
func (qm *ServerMgr) DeleteJobByUser(jobid string, u *user.User, full bool) (err error) {
	var job *Job
	job, err = GetJob(jobid)
	if err != nil {
		return
	}
	// User must have delete permissions on job or be job owner or be an admin
	rights := job.ACL.Check(u.Uuid)
	if job.ACL.Owner != u.Uuid && rights["delete"] == false && u.Admin == false {
		return errors.New(e.UnAuth)
	}
	if err = job.SetState(JOB_STAT_DELETED, nil); err != nil {
		return
	}
	//delete queueing workunits
	var workunit_list []*Workunit
	workunit_list, err = qm.workQueue.GetAll()
	if err != nil {
		return
	}
	for _, workunit := range workunit_list {
		workid := workunit.Workunit_Unique_Identifier
		workunit_jobid := workid.JobId
		//parentid, _ := GetJobIdByWorkId(workid)
		if jobid == workunit_jobid {
			qm.workQueue.Delete(workid)
		}
	}
	//delete parsed tasks
	for i := 0; i < len(job.TaskList()); i++ {
		//taskID := fmt.Sprintf("%s_%d", jobid, i)
		var taskID Task_Unique_Identifier
		taskID, err = New_Task_Unique_Identifier(jobid, strconv.Itoa(i)) // TODO that will not work
		if err != nil {
			return
		}
		qm.TaskMap.Delete(taskID)
	}
	qm.removeActJob(jobid)
	//qm.removeSusJob(jobid)
	// delete from job map
	if err = JM.Delete(jobid, true); err != nil {
		return
	}
	// really delete it !
	if full {
		return job.Delete()
	} else {
		logger.Event(event.JOB_DELETED, "jobid="+jobid)
	}
	return
}

func (qm *ServerMgr) DeleteSuspendedJobsByUser(u *user.User, full bool) (num int) {
	for id := range qm.GetSuspendJobs() {
		if err := qm.DeleteJobByUser(id, u, full); err == nil {
			num += 1
		}
	}
	return
}

func (qm *ServerMgr) ResumeSuspendedJobsByUser(u *user.User) (num int) {
	for id := range qm.GetSuspendJobs() {
		if err := qm.ResumeSuspendedJobByUser(id, u); err == nil {
			num += 1
		}
	}
	return
}

//delete jobs in db with "queued" or "in-progress" state but not in the queue (zombie jobs) that user has access to
func (qm *ServerMgr) DeleteZombieJobsByUser(u *user.User, full bool) (num int) {
	dbjobs := new(Jobs)
	q := bson.M{}
	q["state"] = bson.M{"in": JOB_STATS_ACTIVE}
	if err := dbjobs.GetAll(q, "info.submittime", "asc", false); err != nil {
		logger.Error("DeleteZombieJobs()->GetAllLimitOffset():" + err.Error())
		return
	}
	for _, dbjob := range *dbjobs {
		if !qm.isActJob(dbjob.ID) {
			if err := qm.DeleteJobByUser(dbjob.ID, u, full); err == nil {
				num += 1
			}
		}
	}
	return
}

//resubmit a suspended job if the user is authorized
func (qm *ServerMgr) ResumeSuspendedJobByUser(id string, u *user.User) (err error) {
	//Load job by id
	dbjob, err := GetJob(id)
	if err != nil {
		err = errors.New("(ResumeSuspendedJobByUser) failed to load job " + err.Error())
		return
	}

	jobState, err := dbjob.GetState(true)
	if err != nil {
		err = errors.New("(ResumeSuspendedJobByUser) failed to get job state " + err.Error())
		return
	}

	// User must have write permissions on job or be job owner or be an admin
	rights := dbjob.ACL.Check(u.Uuid)
	if dbjob.ACL.Owner != u.Uuid && rights["write"] == false && u.Admin == false {
		err = errors.New(e.UnAuth)
		return
	}

	if jobState != JOB_STAT_SUSPEND {
		err = errors.New("(ResumeSuspendedJobByUser) job " + id + " is not in 'suspend' status")
		return
	}
	logger.Debug(1, "resumeing job=%s, state=%s", id, jobState)

	tasks, err := dbjob.GetTasks()
	if err != nil {
		err = errors.New("(ResumeSuspendedJobByUser) failed to get job tasks " + err.Error())
		return
	}

	for _, task := range tasks {
		taskState, serr := task.GetState()
		if serr != nil {
			err = errors.New("(ResumeSuspendedJobByUser) failed to get task state " + serr.Error())
			return
		}
		if contains(TASK_STATS_RESET, taskState) {
			logger.Debug(1, "(ResumeSuspendedJobByUser/ResetTaskTrue) task=%s, state=%s", task.ID, taskState)
			err = task.ResetTaskTrue("Resume")
			if err != nil {
				err = errors.New("(ResumeSuspendedJobByUser) failed to reset task " + err.Error())
				return
			}
		}
	}

	err = dbjob.IncrementResumed(1)
	if err != nil {
		err = errors.New("(ResumeSuspendedJobByUser) failed to incremenet job resumed " + err.Error())
		return
	}

	err = dbjob.SetState(JOB_STAT_QUEUING, nil)
	if err != nil {
		err = fmt.Errorf("(ResumeSuspendedJobByUser) UpdateJobState: %s", err.Error())
		return
	}
	err = qm.EnqueueTasksByJobId(id, "ResumeSuspendedJobByUser")
	if err != nil {
		err = errors.New("(ResumeSuspendedJobByUser) failed to enqueue job " + err.Error())
		return
	}
	logger.Debug(1, "Resumed job %s", id)
	return
}

//recover a job in db that is missing from queue (caused by server restarting)
func (qm *ServerMgr) RecoverJob(id string, job *Job) (recovered bool, err error) {
	// job by id or object
	if job != nil {
		id = job.ID
	} else {
		job, err = GetJob(id)
		if err != nil {
			err = errors.New("(RecoverJob) failed to load job " + err.Error())
			return
		}
	}

	if qm.isActJob(id) {
		// already acive, skip
		return
	}

	jobState, err := job.GetState(true)
	if err != nil {
		err = errors.New("(RecoverJob) failed to get job state " + err.Error())
		return
	}

	if jobState == JOB_STAT_SUSPEND {
		// just add suspended jobs to in-memory map
		err = JM.Add(job)
		if err != nil {
			err = errors.New("(RecoverJob) JM.Add failed " + err.Error())
			return
		}
	} else {
		if jobState == JOB_STAT_COMPLETED || jobState == JOB_STAT_DELETED || jobState == JOB_STAT_FAILED_PERMANENT {
			// unrecoverable, skip
			return
		}
		tasks, terr := job.GetTasks()
		if terr != nil {
			err = errors.New("(RecoverJob) failed to get job tasks " + terr.Error())
			return
		}
		for _, task := range tasks {
			taskState, serr := task.GetState()
			if serr != nil {
				err = errors.New("(RecoverJob) failed to get task state " + serr.Error())
				return
			}
			if contains(TASK_STATS_RESET, taskState) {
				logger.Debug(1, "(RecoverJob/ResetTaskTrue) task=%s, state=%s", task.ID, taskState)
				err = task.ResetTaskTrue("Recover")
				if err != nil {
					err = errors.New("(RecoverJob) failed to reset task " + err.Error())
					return
				}
			}
		}
		err = job.SetState(JOB_STAT_QUEUING, nil)
		if err != nil {
			err = errors.New("(RecoverJob) UpdateJobState: " + err.Error())
			return
		}
		err = qm.EnqueueTasksByJobId(id, "RecoverJob")
		if err != nil {
			err = errors.New("(RecoverJob) failed to enqueue job: " + err.Error())
			return
		}
	}
	recovered = true
	logger.Debug(1, "(RecoverJob) done job=%s", id)
	return
}

//recover jobs not completed before awe-server restarts
func (qm *ServerMgr) RecoverJobs() (recovered int, total int, err error) {
	//Get jobs to be recovered from db whose states are recoverable
	dbjobs := new(Jobs)
	q := bson.M{}
	q["state"] = bson.M{"$in": JOB_STATS_TO_RECOVER}
	if conf.RECOVER_MAX > 0 {
		logger.Info("Recover %d jobs...", conf.RECOVER_MAX)
		if _, err = dbjobs.GetPaginated(q, conf.RECOVER_MAX, 0, "info.priority", "desc", true); err != nil {
			logger.Error("(RecoverJobs) (GetPaginated) " + err.Error())
			return
		}
	} else {
		logger.Info("Recover all jobs")
		if err = dbjobs.GetAll(q, "info.submittime", "asc", true); err != nil {
			logger.Error("(RecoverJobs) (GetAll) " + err.Error())
			return
		}
	}
	total = dbjobs.Length()
	//Locate the job script and parse tasks for each job
	for _, dbjob := range *dbjobs {
		pipeline := "missing"
		if dbjob.Info != nil {
			pipeline = dbjob.Info.Pipeline
		}
		logger.Debug(1, "recovering %d: job=%s, state=%s, pipeline=%s", recovered+1, dbjob.ID, dbjob.State, pipeline)
		isRecovered, rerr := qm.RecoverJob("", dbjob)
		if rerr != nil {
			logger.Error(fmt.Sprintf("(RecoverJobs) job=%s failed: %s", dbjob.ID, rerr.Error()))
			continue
		}
		if isRecovered {
			recovered += 1
		}
	}
	return
}

//recompute job from specified task stage
func (qm *ServerMgr) RecomputeJob(jobid string, task_stage string) (err error) {
	if qm.isActJob(jobid) {
		err = errors.New("(RecomputeJob) job " + jobid + " is already active")
		return
	}
	//Load job by id
	dbjob, err := GetJob(jobid)
	if err != nil {
		err = errors.New("(RecomputeJob) failed to load job " + err.Error())
		return
	}

	jobState, err := dbjob.GetState(true)
	if err != nil {
		err = errors.New("(RecomputeJob) failed to get job state " + err.Error())
		return
	}

	if jobState != JOB_STAT_COMPLETED && jobState != JOB_STAT_SUSPEND {
		err = errors.New("(RecomputeJob) job " + jobid + " is not in 'completed' or 'suspend' status")
		return
	}
	logger.Debug(1, "recomputing: job=%s, state=%s", jobid, jobState)

	from_taskID := fmt.Sprintf("%s_%s", jobid, task_stage)
	remain_steps := 0
	found := false

	tasks, err := dbjob.GetTasks()
	if err != nil {
		err = errors.New("(RecomputeJob) failed to get job tasks " + err.Error())
		return
	}

	for _, task := range tasks {
		task_str, terr := task.String()
		if terr != nil {
			err = errors.New("(RecomputeJob) failed to get task ID string " + terr.Error())
			return
		}
		if task_str == from_taskID {
			logger.Debug(1, "(RecomputeJob/ResetTaskTrue) task=%s, state=%s", task_str, task.State)
			err = task.ResetTaskTrue("Recompute")
			if err != nil {
				err = errors.New("(RecomputeJob) failed to reset task " + err.Error())
				return
			}
			found = true
			remain_steps += 1
		}
	}
	if !found {
		return errors.New("(RecomputeJob) task not found: " + from_taskID)
	}

	for _, task := range tasks {
		task_str, terr := task.String()
		if terr != nil {
			err = errors.New("(RecomputeJob) failed to get task ID string " + terr.Error())
			return
		}
		is_ancest, aerr := isAncestor(dbjob, task_str, from_taskID)
		if aerr != nil {
			err = errors.New("(RecomputeJob) failed to determine if task is ancestor " + aerr.Error())
			return
		}
		taskState, serr := task.GetState()
		if serr != nil {
			err = errors.New("(RecomputeJob) failed to get task state " + serr.Error())
			return
		}
		if is_ancest || contains(TASK_STATS_RESET, taskState) {
			logger.Debug(1, "(RecomputeJob/ResetTaskTrue) task=%s, state=%s", task_str, taskState)
			err = task.ResetTaskTrue("Recompute")
			if err != nil {
				err = errors.New("(RecomputeJob) failed to reset task " + err.Error())
				return
			}
			remain_steps += 1
		}
	}

	err = dbjob.IncrementResumed(1)
	if err != nil {
		err = errors.New("(RecomputeJob) failed to incremenet job resumed " + err.Error())
		return
	}

	err = dbjob.SetState(JOB_STAT_QUEUING, nil)
	if err != nil {
		err = fmt.Errorf("(RecomputeJob) UpdateJobState: %s", err.Error())
		return
	}
	err = qm.EnqueueTasksByJobId(jobid, "RecomputeJob")
	if err != nil {
		err = errors.New("(RecomputeJob) failed to enqueue job " + err.Error())
		return
	}
	logger.Debug(1, "Recomputed job %s from task %s", jobid, task_stage)
	return
}

//ResubmitJob recompute job from beginning
func (qm *ServerMgr) ResubmitJob(jobid string) (err error) {
	if qm.isActJob(jobid) {
		err = errors.New("(ResubmitJob) job " + jobid + " is already active")
		return
	}
	//Load job by id
	job, err := GetJob(jobid)
	if err != nil {
		err = errors.New("(ResubmitJob) failed to load job " + err.Error())
		return
	}

	jobState, err := job.GetState(true)
	if err != nil {
		err = errors.New("(ResubmitJob) failed to get job state " + err.Error())
		return
	}

	if jobState != JOB_STAT_COMPLETED && jobState != JOB_STAT_SUSPEND {
		err = errors.New("(ResubmitJob) job " + jobid + " is not in 'completed' or 'suspend' status")
		return
	}
	logger.Debug(1, "resubmitting: job=%s, state=%s", jobid, jobState)

	//remain_steps := 0
	tasks, err := job.GetTasks()
	if err != nil {
		err = errors.New("(ResubmitJob) failed to get job tasks " + err.Error())
		return
	}

	for _, task := range tasks {
		logger.Debug(1, "(ResubmitJob/ResetTaskTrue) task=%s, state=%s", task.ID, task.State)
		err = task.ResetTaskTrue("Resubmit")
		if err != nil {
			err = errors.New("(ResubmitJob) failed to reset task " + err.Error())
			return
		}
		//	remain_steps += 1
	}

	err = job.IncrementResumed(1)
	if err != nil {
		err = errors.New("(ResubmitJob) failed to incremenet job resumed " + err.Error())
		return
	}

	err = job.SetState(JOB_STAT_QUEUING, nil)
	if err != nil {
		err = fmt.Errorf("(ResubmitJob) UpdateJobState: %s", err.Error())
		return
	}
	err = qm.EnqueueTasksByJobId(jobid, "ResubmitJob")
	if err != nil {
		err = errors.New("(ResubmitJob) failed to enqueue job " + err.Error())
		return
	}
	logger.Debug(1, "Restarted job %s from beginning", jobid)
	return
}

func isAncestor(job *Job, taskId string, testId string) (result bool, err error) {
	if taskId == testId {
		result = false
		return
	}
	idx := -1
	for i, task := range job.Tasks {
		var task_str string
		task_str, err = task.String()
		if err != nil {
			err = fmt.Errorf("(isAncestor) task.String returned: %s", err.Error())
			return
		}
		if task_str == taskId {
			idx = i
			break
		}
	}
	if idx == -1 {
		result = false
		return
	}

	task := job.Tasks[idx]
	if len(task.DependsOn) == 0 {
		result = false
		return
	}
	if contains(task.DependsOn, testId) {
		result = true
		return
	} else {
		for _, t := range task.DependsOn {
			return isAncestor(job, t, testId)
		}
	}
	result = false
	return
}

//update tokens for in-memory data structures
func (qm *ServerMgr) UpdateQueueToken(job *Job) (err error) {
	//jobID := job.ID
	for _, task := range job.Tasks {
		taskID, _ := task.GetID("UpdateQueueToken")
		mtask, ok, err := qm.TaskMap.Get(taskID, true)
		if err != nil {
			return err
		}
		if ok {
			mtask.setTokenForIO(true)
		}
	}
	return
}

//---end of job methods

// CreateJobPerf ---perf related methods
func (qm *ServerMgr) CreateJobPerf(jobid string) {
	if !qm.isActJob(jobid) {
		qm.putActJob(NewJobPerf(jobid))
	}
}

// UpdateJobPerfStartTime _
func (qm *ServerMgr) UpdateJobPerfStartTime(jobid string) {
	if perf, ok := qm.getActJob(jobid); ok {
		now := time.Now().Unix()
		perf.Start = now
		qm.putActJob(perf)
	}
	return
}

// FinalizeJobPerf _
func (qm *ServerMgr) FinalizeJobPerf(jobid string) {
	if perf, ok := qm.getActJob(jobid); ok {
		now := time.Now().Unix()
		perf.End = now
		perf.Resp = now - perf.Queued
		qm.putActJob(perf)
	}
	return
}

// CreateTaskPerf _
func (qm *ServerMgr) CreateTaskPerf(task *Task) (err error) {
	jobid := task.JobId
	//taskid := task.String()
	if perf, ok := qm.getActJob(jobid); ok {
		var task_str string
		task_str, err = task.String()
		if err != nil {
			err = fmt.Errorf("() task.String returned: %s", err.Error())
			return
		}
		perf.Ptasks[task_str] = NewTaskPerf(task_str)
		qm.putActJob(perf)
	}
	return
}

// UpdateTaskPerfStartTime _
func (qm *ServerMgr) UpdateTaskPerfStartTime(task *Task) (err error) {
	jobid := task.JobId

	if jobperf, ok := qm.getActJob(jobid); ok {
		var task_str string
		task_str, err = task.String()
		if err != nil {
			err = fmt.Errorf("() task.String returned: %s", err.Error())
			return
		}
		if taskperf, ok := jobperf.Ptasks[task_str]; ok {
			now := time.Now().Unix()
			taskperf.Start = now
			qm.putActJob(jobperf)
		}
	}
	return
}

// FinalizeTaskPerf TODO evaluate err
func (qm *ServerMgr) FinalizeTaskPerf(task *Task) (err error) {
	//jobid, _ := GetJobIdByTaskId(task.Id)
	jobid, err := task.GetJobID()
	if err != nil {
		return
	}
	if jobperf, ok := qm.getActJob(jobid); ok {
		//combined_id := task.String()
		var task_str string
		task_str, err = task.String()
		if err != nil {
			err = fmt.Errorf("() task.String returned: %s", err.Error())
			return
		}

		if taskperf, ok := jobperf.Ptasks[task_str]; ok {
			now := time.Now().Unix()
			taskperf.End = now
			taskperf.Resp = now - taskperf.Queued

			for _, io := range task.Inputs {
				taskperf.InFileSizes = append(taskperf.InFileSizes, io.Size)
			}
			for _, io := range task.Outputs {
				taskperf.OutFileSizes = append(taskperf.OutFileSizes, io.Size)
			}
			qm.putActJob(jobperf)
			return
		}
	}
	return
}

func (qm *ServerMgr) CreateWorkPerf(id Workunit_Unique_Identifier) (err error) {
	if !conf.PERF_LOG_WORKUNIT {
		return
	}
	//workid := id.String()
	jobid := id.JobId
	jobperf, ok := qm.getActJob(jobid)
	if !ok {
		err = fmt.Errorf("(CreateWorkPerf) job perf not found: %s", jobid)
		return
	}
	var workStr string
	workStr, err = id.String()
	if err != nil {
		err = fmt.Errorf("(CreateWorkPerf) id.String() returned: %s", err.Error())
		return
	}
	jobperf.Pworks[workStr] = NewWorkPerf()
	//fmt.Println("write jobperf.Pworks: " + workStr)
	qm.putActJob(jobperf)

	return
}

func (qm *ServerMgr) FinalizeWorkPerf(id Workunit_Unique_Identifier, reportfile string) (err error) {
	if !conf.PERF_LOG_WORKUNIT {
		return
	}
	workperf := new(WorkPerf)
	jsonstream, err := ioutil.ReadFile(reportfile)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(jsonstream, workperf); err != nil {
		return err
	}
	jobid := id.JobId
	jobperf, ok := qm.getActJob(jobid)
	if !ok {
		return errors.New("(FinalizeWorkPerf) job perf not found:" + jobid)
	}
	//workid := id.String()
	var workStr string
	workStr, err = id.String()
	if err != nil {
		err = fmt.Errorf("(FinalizeWorkPerf) workid.String() returned: %s", err.Error())
		return
	}
	if _, ok := jobperf.Pworks[workStr]; !ok {
		for key, _ := range jobperf.Pworks {
			fmt.Println("FinalizeWorkPerf jobperf.Pworks: " + key)
		}
		return errors.New("(FinalizeWorkPerf) work perf not found:" + workStr)
	}

	workperf.Queued = jobperf.Pworks[workStr].Queued
	workperf.Done = time.Now().Unix()
	workperf.Resp = workperf.Done - workperf.Queued
	jobperf.Pworks[workStr] = workperf
	qm.putActJob(jobperf)
	os.Remove(reportfile)
	return
}

func (qm *ServerMgr) LogJobPerf(jobid string) {
	if perf, ok := qm.getActJob(jobid); ok {
		perfstr, _ := json.Marshal(perf)
		logger.Perf(string(perfstr)) //write into perf log
		dbUpsert(perf)               //write into mongodb
	}
}

//---end of perf related methods

func (qm *ServerMgr) FetchPrivateEnv(id Workunit_Unique_Identifier, clientid string) (env map[string]string, err error) {
	//precheck if the client is registered
	client, ok, err := qm.GetClient(clientid, true)
	if err != nil {
		return
	}
	if !ok {
		return env, errors.New(e.ClientNotFound)
	}

	is_suspended, err := client.GetSuspended(true)
	if err != nil {
		return
	}
	if is_suspended {
		err = errors.New(e.ClientSuspended)
		return
	}
	//jobid := id.JobId
	//taskid := id.TaskName

	//job, err := GetJob(jobid)

	task, ok, err := qm.TaskMap.Get(id.Task_Unique_Identifier, true)
	if err != nil {
		err = fmt.Errorf("(FetchPrivateEnv) qm.TaskMap.Get returned: %s", err.Error())
		return
	}

	if !ok {
		//var task_str string
		//task_str, err = task.String()
		//if err != nil {
		//	err = fmt.Errorf("(FetchPrivateEnv) task.String returned: %s", err.Error())
		//	return
		//}
		err = fmt.Errorf("(FetchPrivateEnv) task not found in qm.TaskMap")
		return
	}

	env = task.Cmd.Environ.Private
	return
	//env, err = dbGetPrivateEnv(jobid, taskid)
	//if err != nil {
	//	return
	//}

	//return
}
