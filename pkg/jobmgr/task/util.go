// Copyright (c) 2019 Uber Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package task

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/uber/peloton/.gen/mesos/v1"
	"github.com/uber/peloton/.gen/peloton/api/v0/job"
	"github.com/uber/peloton/.gen/peloton/api/v0/peloton"
	"github.com/uber/peloton/.gen/peloton/api/v0/task"
	v1alphapeloton "github.com/uber/peloton/.gen/peloton/api/v1alpha/peloton"
	"github.com/uber/peloton/.gen/peloton/private/hostmgr/hostsvc"

	"github.com/uber/peloton/pkg/common"
	"github.com/uber/peloton/pkg/common/taskconfig"
	"github.com/uber/peloton/pkg/common/util"
	taskutil "github.com/uber/peloton/pkg/jobmgr/util/task"

	log "github.com/sirupsen/logrus"
	"go.uber.org/yarpc/yarpcerrors"
	"golang.org/x/time/rate"
)

const (
	// timeout for the orphan task kill call
	_defaultKillTaskActionTimeout = 5 * time.Second
	_initialRunID                 = 1
)

// CreateInitializingTask for insertion into the storage layer, before being
// enqueued.
func CreateInitializingTask(jobID *peloton.JobID, instanceID uint32, jobConfig *job.JobConfig) *task.RuntimeInfo {
	mesosTaskID := util.CreateMesosTaskID(jobID, instanceID, _initialRunID)
	healthState := taskutil.GetInitialHealthState(taskconfig.Merge(
		jobConfig.GetDefaultConfig(),
		jobConfig.GetInstanceConfig()[instanceID]))

	runtime := &task.RuntimeInfo{
		MesosTaskId:          mesosTaskID,
		DesiredMesosTaskId:   mesosTaskID,
		State:                task.TaskState_INITIALIZED,
		ConfigVersion:        jobConfig.GetChangeLog().GetVersion(),
		DesiredConfigVersion: jobConfig.GetChangeLog().GetVersion(),
		GoalState:            GetDefaultTaskGoalState(jobConfig.GetType()),
		ResourceUsage:        CreateEmptyResourceUsageMap(),
		Healthy:              healthState,
	}
	return runtime
}

// GetDefaultTaskGoalState from the job type.
func GetDefaultTaskGoalState(jobType job.JobType) task.TaskState {
	switch jobType {
	case job.JobType_SERVICE:
		return task.TaskState_RUNNING

	default:
		return task.TaskState_SUCCEEDED
	}
}

// KillOrphanTask kills a non-stateful Mesos task with unterminated state
func KillOrphanTask(
	ctx context.Context,
	hostmgrClient hostsvc.InternalHostServiceYARPCClient,
	taskInfo *task.TaskInfo) error {

	// TODO(chunyang.shen): store the stateful info into cache instead of going to DB to fetch config
	if util.IsTaskHasValidVolume(taskInfo) {
		// Do not kill stateful orphan task.
		return nil
	}

	state := taskInfo.GetRuntime().GetState()
	mesosTaskID := taskInfo.GetRuntime().GetMesosTaskId()
	agentID := taskInfo.GetRuntime().GetAgentID()

	// Only kill task if state is not terminal.
	if !util.IsPelotonStateTerminal(state) && mesosTaskID != nil {
		var err error
		if state == task.TaskState_KILLING {
			err = ShutdownMesosExecutor(ctx, hostmgrClient, mesosTaskID, agentID, nil)
		} else {
			err = KillTask(ctx, hostmgrClient, mesosTaskID, "", nil)
		}
		if err != nil {
			log.WithError(err).
				WithField("orphan_task_id", mesosTaskID).
				Error("failed to kill orphan task")
		}
		return err
	}
	return nil
}

// KillTask kills a task given its mesos task ID
func KillTask(
	ctx context.Context,
	hostmgrClient hostsvc.InternalHostServiceYARPCClient,
	taskID *mesos_v1.TaskID,
	hostToReserve string,
	rateLimiter *rate.Limiter,
) error {
	newCtx := ctx
	_, ok := ctx.Deadline()
	if !ok {
		var cancelFunc context.CancelFunc
		newCtx, cancelFunc = context.WithTimeout(context.Background(), _defaultKillTaskActionTimeout)
		defer cancelFunc()
	}

	if len(hostToReserve) != 0 {
		return killAndReserveHost(newCtx, hostmgrClient, taskID, hostToReserve, rateLimiter)
	}

	return killHost(newCtx, hostmgrClient, taskID, rateLimiter)
}

func killHost(
	ctx context.Context,
	hostmgrClient hostsvc.InternalHostServiceYARPCClient,
	taskID *mesos_v1.TaskID,
	rateLimiter *rate.Limiter,
) error {
	if rateLimiter != nil && !rateLimiter.Allow() {
		return yarpcerrors.ResourceExhaustedErrorf("rate limit reached for kill task")
	}

	req := &hostsvc.KillTasksRequest{
		TaskIds: []*mesos_v1.TaskID{taskID},
	}
	res, err := hostmgrClient.KillTasks(ctx, req)
	if err != nil {
		return err
	} else if e := res.GetError(); e != nil {
		switch {
		case e.KillFailure != nil:
			return fmt.Errorf(e.KillFailure.Message)
		case e.InvalidTaskIDs != nil:
			return fmt.Errorf(e.InvalidTaskIDs.Message)
		default:
			return fmt.Errorf(e.String())
		}
	}
	return nil
}

func killAndReserveHost(
	ctx context.Context,
	hostmgrClient hostsvc.InternalHostServiceYARPCClient,
	mesosTaskID *mesos_v1.TaskID,
	hostToReserve string,
	rateLimiter *rate.Limiter,
) error {
	if rateLimiter != nil && !rateLimiter.Allow() {
		return yarpcerrors.ResourceExhaustedErrorf("rate limit reached for kill and reserve task")
	}

	taskID, err := util.ParseTaskIDFromMesosTaskID(mesosTaskID.GetValue())
	if err != nil {
		return err
	}

	req := &hostsvc.KillAndReserveTasksRequest{
		Entries: []*hostsvc.KillAndReserveTasksRequest_Entry{
			{Id: &peloton.TaskID{Value: taskID}, TaskId: mesosTaskID, HostToReserve: hostToReserve},
		},
	}
	res, err := hostmgrClient.KillAndReserveTasks(ctx, req)
	if err != nil {
		return err
	} else if e := res.GetError(); e != nil {
		switch {
		case e.KillFailure != nil:
			return fmt.Errorf(e.KillFailure.Message)
		case e.InvalidTaskIDs != nil:
			return fmt.Errorf(e.InvalidTaskIDs.Message)
		default:
			return fmt.Errorf(e.String())
		}
	}
	return nil
}

// ShutdownMesosExecutor shutdown a executor given its executor ID and agent ID
func ShutdownMesosExecutor(
	ctx context.Context,
	hostmgrClient hostsvc.InternalHostServiceYARPCClient,
	taskID *mesos_v1.TaskID,
	agentID *mesos_v1.AgentID,
	rateLimiter *rate.Limiter,
) error {
	if rateLimiter != nil && !rateLimiter.Allow() {
		return yarpcerrors.ResourceExhaustedErrorf("rate limit reached for executor shutdown")
	}

	req := &hostsvc.ShutdownExecutorsRequest{
		Executors: []*hostsvc.ExecutorOnAgent{
			{
				ExecutorId: &mesos_v1.ExecutorID{Value: taskID.Value},
				AgentId:    agentID,
			},
		},
	}

	res, err := hostmgrClient.ShutdownExecutors(ctx, req)

	if err != nil {
		return err
	} else if e := res.GetError(); e != nil {
		switch {
		case e.ShutdownFailure != nil:
			return fmt.Errorf(e.ShutdownFailure.Message)
		case e.InvalidExecutors != nil:
			return fmt.Errorf(e.InvalidExecutors.Message)
		default:
			return fmt.Errorf(e.String())
		}
	}

	return nil
}

// CreateSecretsFromVolumes creates secret proto message list from the given
// list of secret volumes.
func CreateSecretsFromVolumes(
	secretVolumes []*mesos_v1.Volume) []*peloton.Secret {
	secrets := []*peloton.Secret{}
	for _, volume := range secretVolumes {
		secrets = append(secrets, CreateSecretProto(
			string(volume.GetSource().GetSecret().GetValue().GetData()),
			volume.GetContainerPath(), nil))
	}
	return secrets
}

// CreateSecretProto creates secret proto message from secret-id, path and data
func CreateSecretProto(id, path string, data []byte) *peloton.Secret {
	// base64 encode the secret data
	if len(data) > 0 {
		data = []byte(base64.StdEncoding.EncodeToString(data))
	}
	return &peloton.Secret{
		Id: &peloton.SecretID{
			Value: id,
		},
		Path: path,
		Value: &peloton.Secret_Value{
			Data: data,
		},
	}
}

// CreateV1AlphaSecretProto creates v1alpha secret proto
// message from secret-id, path and data
func CreateV1AlphaSecretProto(id, path string, data []byte) *v1alphapeloton.Secret {
	// base64 encode the secret data
	if len(data) > 0 {
		data = []byte(base64.StdEncoding.EncodeToString(data))
	}
	return &v1alphapeloton.Secret{
		SecretId: &v1alphapeloton.SecretID{
			Value: id,
		},
		Path: path,
		Value: &v1alphapeloton.Secret_Value{
			Data: data,
		},
	}
}

// CreateEmptyResourceUsageMap creates a resource usage map with usage stats
// initialized to 0
func CreateEmptyResourceUsageMap() map[string]float64 {
	return map[string]float64{
		common.CPU:    float64(0),
		common.GPU:    float64(0),
		common.MEMORY: float64(0),
	}
}

// CreateResourceUsageMap creates a resource usage map with usage stats
// calculated as resource limit * duration
func CreateResourceUsageMap(
	resourceConfig *task.ResourceConfig,
	startTimeStr, completionTimeStr string) (map[string]float64, error) {
	cpulimit := resourceConfig.GetCpuLimit()
	gpulimit := resourceConfig.GetGpuLimit()
	memlimit := resourceConfig.GetMemLimitMb()
	resourceUsage := CreateEmptyResourceUsageMap()

	// if start time is "", it means the task did not start so resource usage
	// should be 0 for all resources
	if startTimeStr == "" {
		return resourceUsage, nil
	}

	startTime, err := time.Parse(time.RFC3339Nano, startTimeStr)
	if err != nil {
		return nil, err
	}
	completionTime, err := time.Parse(time.RFC3339Nano, completionTimeStr)
	if err != nil {
		return nil, err
	}

	startTimeUnix := float64(startTime.UnixNano()) /
		float64(time.Second/time.Nanosecond)
	completionTimeUnix := float64(completionTime.UnixNano()) /
		float64(time.Second/time.Nanosecond)

	// update the resource usage map for CPU, GPU and memory usage
	resourceUsage[common.CPU] = (completionTimeUnix - startTimeUnix) * cpulimit
	resourceUsage[common.GPU] = (completionTimeUnix - startTimeUnix) * gpulimit
	resourceUsage[common.MEMORY] =
		(completionTimeUnix - startTimeUnix) * memlimit
	return resourceUsage, nil
}
