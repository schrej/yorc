// Copyright 2018 Bull S.A.S. Atos Technologies - Bull, Rue Jean Jaures, B.P.68, 78340, Les Clayes-sous-Bois, France.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/armon/go-metrics"
	"github.com/hashicorp/consul/api"
	"github.com/pkg/errors"
	"github.com/ystia/yorc/config"
	"github.com/ystia/yorc/deployments"
	"github.com/ystia/yorc/events"
	"github.com/ystia/yorc/helper/consulutil"
	"github.com/ystia/yorc/helper/metricsutil"
	"github.com/ystia/yorc/log"
	"github.com/ystia/yorc/prov"
	"github.com/ystia/yorc/prov/operations"
	"github.com/ystia/yorc/registry"
	"github.com/ystia/yorc/tasks"
	"github.com/ystia/yorc/tosca"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// worker concern is to execute a task execution received from the dispatcher
// It has to poll cancellation flags to stop execution
// In case of workflow task, it runs a workflow step as a task execution and needs to update related task status, and register next step tasks execution if its execution is successful
//FIXME do we need to poll the task status to cancel running execution if another one has failed ?
type worker struct {
	workerPool   chan chan *TaskExecution
	TaskChannel  chan *TaskExecution
	shutdownCh   chan struct{}
	consulClient *api.Client
	cfg          config.Configuration
}

func newWorker(workerPool chan chan *TaskExecution, shutdownCh chan struct{}, consulClient *api.Client, cfg config.Configuration) worker {
	return worker{
		workerPool:   workerPool,
		TaskChannel:  make(chan *TaskExecution),
		shutdownCh:   shutdownCh,
		consulClient: consulClient,
		cfg:          cfg,
	}
}

// Start method starts the run loop for the worker, listening for a quit channel in
// case we need to stop it
func (w worker) Start() {
	go func() {
		for {
			// register the current worker into the worker queue.
			w.workerPool <- w.TaskChannel
			select {
			case task := <-w.TaskChannel:
				// we have received a work request.
				log.Debugf("Worker got Task Execution with id %s", task.TaskID)
				w.handleExecution(task)

			case <-w.shutdownCh:
				// we have received a signal to stop
				log.Printf("Worker received shutdown signal. Exiting...")
				return
			}
		}
	}()
}

func setNodeStatus(ctx context.Context, kv *api.KV, taskID, deploymentID, nodeName, status string) error {
	instancesIDs, err := tasks.GetInstances(kv, taskID, deploymentID, nodeName)
	if err != nil {
		return err
	}

	for _, id := range instancesIDs {
		// Publish status change event
		err := deployments.SetInstanceStateStringWithContextualLogs(ctx, kv, deploymentID, nodeName, id, status)
		if err != nil {
			return err
		}
	}
	return nil
}

func getOperationExecutor(kv *api.KV, deploymentID, artifact string) (prov.OperationExecutor, error) {
	reg := registry.GetRegistry()

	exec, originalErr := reg.GetOperationExecutor(artifact)
	if originalErr == nil {
		return exec, nil
	}
	// Try to get an executor for artifact parent type but return the original error if we do not found any executors
	parentArt, err := deployments.GetParentType(kv, deploymentID, artifact)
	if err != nil {
		return nil, err
	}
	if parentArt != "" {
		exec, err := getOperationExecutor(kv, deploymentID, parentArt)
		if err == nil {
			return exec, nil
		}
	}
	return nil, originalErr
}

func (w worker) monitorTaskForCancellation(ctx context.Context, cancelFunc context.CancelFunc, t *TaskExecution) {
	go func() {
		var lastIndex uint64
		for {
			kvp, qMeta, err := w.consulClient.KV().Get(path.Join(consulutil.TasksPrefix, t.TaskID, ".canceledFlag"), &api.QueryOptions{WaitIndex: lastIndex})

			select {
			case <-ctx.Done():
				log.Debugln("[TASK MONITOR] Task monitor exiting as Task Execution ended...")
				return
			default:
			}

			if qMeta != nil {
				lastIndex = qMeta.LastIndex
			}

			if err == nil && kvp != nil {
				if strings.ToLower(string(kvp.Value)) == "true" {
					log.Debugln("[TASK MONITOR] Task cancellation requested.")
					t.setTaskStatus(ctx, tasks.TaskStatusCANCELED)
					cancelFunc()
					return
				}
			}
		}
	}()
}

// cleanupScaledDownNodes removes nodes instances from Consul
func (w worker) cleanupScaledDownNodes(t *TaskExecution) error {
	kv := w.consulClient.KV()

	nodes, err := tasks.GetTaskRelatedNodes(kv, t.TaskID)
	if err != nil {
		return err
	}
	for _, node := range nodes {
		var instances []string
		instances, err = tasks.GetInstances(kv, t.TaskID, t.TargetID, node)
		if err != nil {
			return err
		}
		for _, instance := range instances {
			err = deployments.DeleteInstance(kv, t.TargetID, node, instance)
			if err != nil {
				return err
			}
			err = deployments.DeleteRelationshipInstance(kv, t.TargetID, node, instance)
			if err != nil {
				return err
			}
		}

	}
	return nil
}

func (w worker) setDeploymentStatus(ctx context.Context, deploymentID string, status deployments.DeploymentStatus) {
	p := &api.KVPair{Key: path.Join(consulutil.DeploymentKVPrefix, deploymentID, "status"), Value: []byte(fmt.Sprint(status))}
	kv := w.consulClient.KV()
	kv.Put(p, nil)
	events.PublishAndLogDeploymentStatusChange(ctx, kv, deploymentID, strings.ToLower(status.String()))
}

func (w worker) handleExecution(t *TaskExecution) {
	metrics.MeasureSince([]string{"TaskExecution", "wait"}, t.creationDate)
	kv := w.consulClient.KV()

	// Fill log optional fields for log registration
	wfName, _ := tasks.GetTaskData(kv, t.TaskID, "workflowName")
	logOptFields := events.LogOptionalFields{
		events.WorkFlowID:  wfName,
		events.ExecutionID: t.TaskID,
	}
	ctx := events.NewContext(context.Background(), logOptFields)

	ctx, cancelFunc := context.WithCancel(ctx)
	defer t.releaseLock()
	defer cancelFunc()

	if err := t.setTaskStatus(ctx, tasks.TaskStatusRUNNING); err != nil {
		log.Printf("Failed to set task status to running due to err:%v", err)
		return
	}

	w.monitorTaskForCancellation(ctx, cancelFunc, t)
	defer func(t *TaskExecution, start time.Time) {
		metrics.IncrCounter(metricsutil.CleanupMetricKey([]string{"TaskExecution", t.TargetID, t.TaskType.String()}), 1)
		metrics.MeasureSince(metricsutil.CleanupMetricKey([]string{"TaskExecution", t.TargetID, t.TaskType.String()}), start)
	}(t, time.Now())
	switch t.TaskType {
	case tasks.TaskTypeDeploy:
		w.setDeploymentStatus(ctx, t.TargetID, deployments.DEPLOYMENT_IN_PROGRESS)
		err := w.runWorkflowStep(ctx, t, "install", false)
		if err != nil {
			w.setDeploymentStatus(ctx, t.TargetID, deployments.DEPLOYMENT_FAILED)
			return
		}
		w.setDeploymentStatus(ctx, t.TargetID, deployments.DEPLOYED)
	case tasks.TaskTypeUnDeploy, tasks.TaskTypePurge:
		status, err := deployments.GetDeploymentStatus(kv, t.TargetID)
		if err != nil {
			log.Printf("Deployment id: %q, Task id: %q, Failed to get deployment status: %+v", t.TargetID, t.TaskID, err)
			t.setTaskStatus(ctx, tasks.TaskStatusFAILED)
			return
		}
		if status != deployments.UNDEPLOYED {
			w.setDeploymentStatus(ctx, t.TargetID, deployments.UNDEPLOYMENT_IN_PROGRESS)
			err := w.runWorkflowStep(ctx, t, "uninstall", true)
			if err != nil {
				w.setDeploymentStatus(ctx, t.TargetID, deployments.UNDEPLOYMENT_FAILED)
				return
			}
			w.setDeploymentStatus(ctx, t.TargetID, deployments.UNDEPLOYED)
		}

		if t.TaskType == tasks.TaskTypePurge {
			_, err := kv.DeleteTree(path.Join(consulutil.DeploymentKVPrefix, t.TargetID), nil)
			if err != nil {
				log.Printf("Deployment id: %q, Task id: %q, Failed to purge deployment definition: %+v", t.TargetID, t.TaskID, err)
				t.setTaskStatus(ctx, tasks.TaskStatusFAILED)
				return
			}
			tasksList, err := tasks.GetTasksIdsForTarget(kv, t.TargetID)
			if err != nil {
				log.Printf("Deployment id: %q, Task id: %q, Failed to purge tasks related to deployment: %+v", t.TargetID, t.TaskID, err)
				t.setTaskStatus(ctx, tasks.TaskStatusFAILED)
				return
			}
			for _, tid := range tasksList {
				if tid != t.TaskID {
					_, err = kv.DeleteTree(path.Join(consulutil.TasksPrefix, tid), nil)
					if err != nil {
						log.Printf("Deployment id: %q, Task id: %q, Failed to purge tasks related to deployment: %+v", t.TargetID, t.TaskID, err)
						t.setTaskStatus(ctx, tasks.TaskStatusFAILED)
						return
					}
				}
				_, err = kv.DeleteTree(path.Join(consulutil.WorkflowsPrefix, tid), nil)
				if err != nil {
					log.Printf("Deployment id: %q, Task id: %q, Failed to purge tasks related to deployment: %+v", t.TargetID, t.TaskID, err)
					t.setTaskStatus(ctx, tasks.TaskStatusFAILED)
					return
				}
			}
			// Delete events tree corresponding to the deployment TaskExecution
			_, err = kv.DeleteTree(path.Join(consulutil.EventsPrefix, t.TargetID), nil)
			if err != nil {
				log.Printf("Deployment id: %q, Task id: %q, Failed to purge events: %+v", t.TargetID, t.TaskID, err)
				t.setTaskStatus(ctx, tasks.TaskStatusFAILED)
				return
			}
			// Delete logs tree corresponding to the deployment TaskExecution
			_, err = kv.DeleteTree(path.Join(consulutil.LogsPrefix, t.TargetID), nil)
			if err != nil {
				log.Printf("Deployment id: %q, Task id: %q, Failed to purge logs: %+v", t.TargetID, t.TaskID, err)
				t.setTaskStatus(ctx, tasks.TaskStatusFAILED)
				return
			}
			err = os.RemoveAll(filepath.Join(w.cfg.WorkingDirectory, "deployments", t.TargetID))
			if err != nil {
				log.Printf("Deployment id: %q, Task id: %q, Failed to purge tasks related to deployment: %+v", t.TargetID, t.TaskID, err)
				t.setTaskStatus(ctx, tasks.TaskStatusFAILED)
				return
			}
			// Now cleanup: mark it as done so nobody will try to run it, clear the processing lock and finally delete the TaskExecution.
			t.setTaskStatus(ctx, tasks.TaskStatusDONE)
			t.releaseLock()
			_, err = kv.DeleteTree(path.Join(consulutil.TasksPrefix, t.TaskID), nil)
			if err != nil {
				log.Printf("Deployment id: %q, Task id: %q, Failed to purge tasks related to deployment: %+v", t.TargetID, t.TaskID, err)
				t.setTaskStatus(ctx, tasks.TaskStatusFAILED)
				return
			}
			return
		}
	case tasks.TaskTypeCustomCommand:
		w.runCustomCommand(ctx, t)
	case tasks.TaskTypeScaleOut:
		w.setDeploymentStatus(ctx, t.TargetID, deployments.SCALING_IN_PROGRESS)
		err := w.runWorkflowStep(ctx, t, "install", false)
		if err != nil {
			w.setDeploymentStatus(ctx, t.TargetID, deployments.DEPLOYMENT_FAILED)
			return
		}
		w.setDeploymentStatus(ctx, t.TargetID, deployments.DEPLOYED)
	case tasks.TaskTypeScaleIn:
		w.setDeploymentStatus(ctx, t.TargetID, deployments.SCALING_IN_PROGRESS)
		err := w.runWorkflowStep(ctx, t, "uninstall", true)
		if err != nil {
			w.setDeploymentStatus(ctx, t.TargetID, deployments.DEPLOYMENT_FAILED)
			return
		}

		// Cleanup
		if err = w.cleanupScaledDownNodes(t); err != nil {
			t.checkAndSetTaskStatus(ctx, tasks.TaskStatusRUNNING, tasks.TaskStatusFAILED)
			log.Printf("%v. Aborting", err)
			w.setDeploymentStatus(ctx, t.TargetID, deployments.DEPLOYMENT_FAILED)
			return
		}
		w.setDeploymentStatus(ctx, t.TargetID, deployments.DEPLOYED)
	case tasks.TaskTypeCustomWorkflow:
		wfName, err := tasks.GetTaskData(kv, t.TaskID, "workflowName")
		if err != nil {
			log.Printf("Deployment id: %q, Task id: %q Failed: %v", t.TargetID, t.TaskID, err)
			log.Debugf("%+v", err)
			t.setTaskStatus(ctx, tasks.TaskStatusFAILED)
			return
		}
		continueOnError, err := tasks.GetTaskData(kv, t.TaskID, "continueOnError")
		if err != nil {
			log.Printf("Deployment id: %q, Task id: %q Failed: %v", t.TargetID, t.TaskID, err)
			log.Debugf("%+v", err)
			t.setTaskStatus(ctx, tasks.TaskStatusFAILED)
			return
		}
		bypassErrors, err := strconv.ParseBool(continueOnError)
		if err != nil {
			log.Printf("Deployment id: %q, Task id: %q Failed to parse continueOnError parameter: %v", t.TargetID, t.TaskID, err)
			log.Debugf("%+v", err)
			t.setTaskStatus(ctx, tasks.TaskStatusFAILED)
			return
		}
		err = w.runWorkflowStep(ctx, t, wfName, bypassErrors)
		if err != nil {
			t.setTaskStatus(ctx, tasks.TaskStatusFAILED)
			log.Printf("%+v", err)
			return
		}
	case tasks.TaskTypeQuery:
		w.runQuery(ctx, t)
	default:
		events.WithContextOptionalFields(ctx).NewLogEntry(events.LogLevelERROR, t.TargetID).RegisterAsString(fmt.Sprintf("Unknown TaskType %d (%s) for TaskExecution with id %q", t.TaskType, t.TaskType.String(), t.TaskID))
		log.Printf("Unknown TaskType %d (%s) for TaskExecution with id %q and targetId %q", t.TaskType, t.TaskType.String(), t.TaskID, t.TargetID)
		t.checkAndSetTaskStatus(ctx, tasks.TaskStatusRUNNING, tasks.TaskStatusFAILED)
		return
	}
}

func (w worker) runCustomCommand(ctx context.Context, t *TaskExecution) {
	kv := w.consulClient.KV()
	commandNameKv, _, err := kv.Get(path.Join(consulutil.TasksPrefix, t.TaskID, "commandName"), nil)
	if err != nil {
		log.Printf("Deployment id: %q, Task id: %q, Failed to get Custom command name: %+v", t.TargetID, t.TaskID, err)
		t.setTaskStatus(ctx, tasks.TaskStatusFAILED)
		return
	}
	if commandNameKv == nil || len(commandNameKv.Value) == 0 {
		log.Printf("Deployment id: %q, Task id: %q, Missing commandName attribute for custom command TaskExecution", t.TargetID, t.TaskID)
		t.setTaskStatus(ctx, tasks.TaskStatusFAILED)
		return
	}

	nodes, err := tasks.GetTaskRelatedNodes(kv, t.TaskID)
	if err != nil {
		log.Printf("Deployment id: %q, Task id: %q, Failed to get Custom command node: %+v", t.TargetID, t.TaskID, err)
		t.setTaskStatus(ctx, tasks.TaskStatusFAILED)
		return
	}
	if len(nodes) != 1 {
		log.Printf("Deployment id: %q, Task id: %q, Expecting custom command TaskExecution to be related to \"1\" node while it is actually related to \"%d\" nodes", t.TargetID, t.TaskID, len(nodes))
		t.setTaskStatus(ctx, tasks.TaskStatusFAILED)
		return
	}

	nodeName := nodes[0]
	commandName := string(commandNameKv.Value)
	nodeType, err := deployments.GetNodeType(w.consulClient.KV(), t.TargetID, nodeName)
	if err != nil {
		log.Printf("Deployment id: %q, Task id: %q, Failed to get Custom command node type: %+v", t.TargetID, t.TaskID, err)
		t.setTaskStatus(ctx, tasks.TaskStatusFAILED)
		return
	}
	op, err := operations.GetOperation(ctx, kv, t.TargetID, nodeName, "custom."+commandName, "", "")
	if err != nil {
		log.Printf("Deployment id: %q, Task id: %q, Command TaskExecution failed for node %q: %+v", t.TargetID, t.TaskID, nodeName, err)
		err = setNodeStatus(ctx, t.kv, t.TaskID, t.TargetID, nodeName, tosca.NodeStateError.String())
		if err != nil {
			log.Printf("Deployment id: %q, Task id: %q, Failed to set status for node %q: %+v", t.TargetID, t.TaskID, nodeName, err)
		}
		t.setTaskStatus(ctx, tasks.TaskStatusFAILED)
		return
	}

	exec, err := getOperationExecutor(kv, t.TargetID, op.ImplementationArtifact)
	if err != nil {
		log.Printf("Deployment id: %q, Task id: %q, Command TaskExecution failed for node %q: %+v", t.TargetID, t.TaskID, nodeName, err)
		err = setNodeStatus(ctx, t.kv, t.TaskID, t.TargetID, nodeName, tosca.NodeStateError.String())
		if err != nil {
			log.Printf("Deployment id: %q, Task id: %q, Failed to set status for node %q: %+v", t.TargetID, t.TaskID, nodeName, err)
		}
		t.setTaskStatus(ctx, tasks.TaskStatusFAILED)
		return
	}
	err = func() error {
		defer metrics.MeasureSince(metricsutil.CleanupMetricKey([]string{"executor", "operation", t.TargetID, nodeType, op.Name}), time.Now())
		return exec.ExecOperation(ctx, w.cfg, t.TaskID, t.TargetID, nodeName, op)
	}()
	if err != nil {
		metrics.IncrCounter(metricsutil.CleanupMetricKey([]string{"executor", "operation", t.TargetID, nodeType, op.Name, "failures"}), 1)
		log.Printf("Deployment id: %q, Task id: %q, Command TaskExecution failed for node %q: %+v", t.TargetID, t.TaskID, nodeName, err)
		err = setNodeStatus(ctx, t.kv, t.TaskID, t.TargetID, nodeName, tosca.NodeStateError.String())
		if err != nil {
			log.Printf("Deployment id: %q, Task id: %q, Failed to set status for node %q: %+v", t.TargetID, t.TaskID, nodeName, err)
		}
		t.setTaskStatus(ctx, tasks.TaskStatusFAILED)
		return
	}
	t.setTaskStatus(ctx, tasks.TaskStatusDONE)
	metrics.IncrCounter(metricsutil.CleanupMetricKey([]string{"executor", "operation", t.TargetID, nodeType, op.Name, "successes"}), 1)
}

func (w worker) runQuery(ctx context.Context, t *TaskExecution) {
	kv := w.consulClient.KV()
	split := strings.Split(t.TargetID, ":")
	if len(split) != 2 {
		log.Printf("Query Task (id: %q): unexpected format for targetID: %q", t.TaskID, t.TargetID)
		t.setTaskStatus(ctx, tasks.TaskStatusFAILED)
		return
	}
	query := split[0]
	target := split[1]

	switch query {
	case "infra_usage":
		var reg = registry.GetRegistry()
		collector, err := reg.GetInfraUsageCollector(target)
		if err != nil {
			log.Printf("Query Task id: %q Failed to retrieve target type: %v", t.TaskID, err)
			log.Debugf("%+v", err)
			t.setTaskStatus(ctx, tasks.TaskStatusFAILED)
			return
		}
		res, err := collector.GetUsageInfo(ctx, w.cfg, t.TaskID, target)
		if err != nil {
			log.Printf("Query Task id: %q Failed to run query: %v", t.TaskID, err)
			log.Debugf("%+v", err)
			t.setTaskStatus(ctx, tasks.TaskStatusFAILED)
			return
		}

		// store resultSet as a JSON
		resultPrefix := path.Join(consulutil.TasksPrefix, t.TaskID, "resultSet")
		if res != nil {
			jsonRes, err := json.Marshal(res)
			if err != nil {
				log.Printf("Failed to marshal infra usage info [%+v]: due to error:%+v", res, err)
				log.Debugf("%+v", err)
				t.setTaskStatus(ctx, tasks.TaskStatusFAILED)
				return
			}
			kvPair := &api.KVPair{Key: resultPrefix, Value: jsonRes}
			if _, err := kv.Put(kvPair, nil); err != nil {
				log.Printf("Query Task id: %q Failed to store result: %v", t.TaskID, errors.Wrap(err, consulutil.ConsulGenericErrMsg))
				log.Debugf("%+v", err)
				t.setTaskStatus(ctx, tasks.TaskStatusFAILED)
				return
			}
		}
	default:
		mess := fmt.Sprintf("Unknown query: %q for Task with id %q", query, t.TaskID)
		events.WithContextOptionalFields(ctx).NewLogEntry(events.LogLevelERROR, t.TargetID).RegisterAsString(mess)
		log.Printf(mess)
		t.checkAndSetTaskStatus(ctx, tasks.TaskStatusRUNNING, tasks.TaskStatusFAILED)
		return
	}
	t.setTaskStatus(ctx, tasks.TaskStatusDONE)
}

func (w worker) runWorkflowStep(ctx context.Context, t *TaskExecution, workflowName string, continueOnError bool) error {
	events.WithContextOptionalFields(ctx).NewLogEntry(events.LogLevelINFO, t.TargetID).RegisterAsString(fmt.Sprintf("Start processing workflow step %s:%s", workflowName, t.step))

	s, err := BuildStep(w.consulClient.KV(), t.TargetID, workflowName, t.step, nil)
	if err != nil {
		t.checkAndSetTaskStatus(ctx, tasks.TaskStatusRUNNING, tasks.TaskStatusFAILED)
		log.Printf("%v. Aborting", err)
		return errors.Wrap(err, consulutil.ConsulGenericErrMsg)
	}
	s.t = t
	err = s.Run(ctx, w.cfg, w.consulClient.KV(), t.TargetID, w.shutdownCh, continueOnError, workflowName, w)
	if err != nil {
		t.checkAndSetTaskStatus(ctx, tasks.TaskStatusRUNNING, tasks.TaskStatusFAILED)
		events.WithContextOptionalFields(ctx).NewLogEntry(events.LogLevelERROR, t.TargetID).RegisterAsString(fmt.Sprintf("Error '%v' happened in workflow %q.", err, workflowName))
		return errors.Wrapf(err, "The workflow %s step %s ended with error:%v", workflowName, t.step, err)
	}

	events.WithContextOptionalFields(ctx).NewLogEntry(events.LogLevelINFO, t.TargetID).RegisterAsString(fmt.Sprintf("DeploymentID:%q, Workflow:%q, step:%q ended without error", t.TargetID, workflowName, t.step))
	return w.notifyNextSteps(ctx, s, workflowName)
}

func (w worker) notifyNextSteps(ctx context.Context, s *Step, workflowName string) error {
	// If step is terminal, we check if workflow is done
	if s.IsTerminal() {
		return w.checkIfWorkflowIsOver(ctx, s.t, workflowName)
	}

	// Register workflow step to handle step statuses for next steps
	ops := GetWorkflowStepsOperations(s.t.TaskID, s.Next)
	ok, response, _, err := w.consulClient.KV().Txn(ops, nil)
	if err != nil {
		return errors.Wrapf(err, "Failed to register executionTasks with TaskID:%q", s.t.TaskID)
	}
	if !ok {
		errs := make([]string, 0)
		for _, e := range response.Errors {
			errs = append(errs, e.What)
		}
		return errors.Wrapf(err, "Failed to register executionTasks with TaskID:%q due to:%s", s.t.TaskID, strings.Join(errs, ", "))
	}
	return nil
}

func (w worker) checkIfWorkflowIsOver(ctx context.Context, t *TaskExecution, workflowName string) error {
	taskSteps, err := tasks.GetTaskRelatedSteps(w.consulClient.KV(), t.TaskID)
	if err != nil {
		return errors.Wrapf(err, "Failed to retrieve workflow step statuses from t with TaskID:%q", t.TaskID)
	}
	cpt := 0
	for _, step := range taskSteps {
		stepStatus, err := tasks.ParseStepStatus(step.Status)
		if err != nil {
			return errors.Wrapf(err, "Failed to retrieve workflow step statuses from t with TaskID:%q", t.TaskID)
		}
		if stepStatus == tasks.StepStatusDONE {
			cpt++
		} else if stepStatus == tasks.StepStatusCANCELED || stepStatus == tasks.StepStatusERROR {
			return errors.Errorf("An error has been detected on other step:%q for workflow:%q, deploymentID:%q. No more steps will be executed", t.step, workflowName, t.TargetID)
		}
	}

	if len(taskSteps) == cpt {
		err = t.checkAndSetTaskStatus(ctx, tasks.TaskStatusRUNNING, tasks.TaskStatusDONE)
		if err != nil {
			return errors.Wrapf(err, "Failed to update task status to DONE with TaskID:%q due to error:%v", t.TaskID, err)
		}
		events.WithContextOptionalFields(ctx).NewLogEntry(events.LogLevelINFO, t.TargetID).RegisterAsString(fmt.Sprintf("Workflow %q ended without error", workflowName))
	}

	return nil
}

func (w worker) registerInlineWorkflow(ctx context.Context, t *TaskExecution, workflowName string) error {
	events.WithContextOptionalFields(ctx).NewLogEntry(events.LogLevelINFO, t.TargetID).RegisterAsString(fmt.Sprintf("Register workflow %q from taskID:%q, deploymentID:%q", workflowName, t.TaskID, t.TargetID))
	wfOps, err := GetWorkflowInitOperations(t.kv, t.TargetID, t.TaskID, workflowName)
	if err != nil {
		return err
	}
	ok, response, _, err := t.kv.Txn(wfOps, nil)
	if err != nil {
		return errors.Wrapf(err, "Failed to register workflow init operations with workflow:%q, targetID:%q, taskID:%q", workflowName, t.TargetID, t.TaskID)
	}
	if !ok {
		errs := make([]string, 0)
		for _, e := range response.Errors {
			errs = append(errs, e.What)
		}
		return errors.Wrapf(err, "Failed to register workflow init operations with workflow:%q, targetID:%q, taskID:%q due to error:%q", workflowName, t.TargetID, t.TaskID, strings.Join(errs, ", "))
	}

	return nil
}
