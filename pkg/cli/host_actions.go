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

package cli

import (
	"fmt"
	"sort"
	"strings"

	host "github.com/uber/peloton/.gen/peloton/api/v0/host"
	host_svc "github.com/uber/peloton/.gen/peloton/api/v0/host/svc"
	pb_task "github.com/uber/peloton/.gen/peloton/api/v0/task"
	"github.com/uber/peloton/.gen/peloton/private/hostmgr/hostsvc"

	"github.com/uber/peloton/pkg/hostmgr/scalar"
)

const (
	hostQueryFormatHeader = "Hostname\tIP\tState\n"
	hostQueryFormatBody   = "%s\t%s\t%s\n"
	hostSeparator         = ","
	getHostsFormatHeader  = "Hostname\tCPU\tGPU\tMEM\tDisk\tState\t Task Hold\n"
	getHostsFormatBody    = "%s\t%.2f\t%.2f\t%.2f MB\t%.2f MB\t%s\t%s\n"
)

// HostMaintenanceStartAction is the action for starting host maintenance. StartMaintenance puts the host(s)
// into DRAINING state by posting a maintenance schedule to Mesos Master. Inverse offers are sent out and
// all future offers from the(se) host(s) are tagged with unavailability (Please check Mesos Maintenance
// Primitives for more info). The hosts are first drained of tasks before they are put into maintenance
// by posting to /machine/down endpoint of Mesos Master.
// The hosts transition from UP to DRAINING and finally to DOWN.
func (c *Client) HostMaintenanceStartAction(hosts string) error {
	hostnames, err := c.ExtractHostnames(hosts, hostSeparator)
	if err != nil {
		return err
	}

	request := &host_svc.StartMaintenanceRequest{
		Hostnames: hostnames,
	}
	_, err = c.hostClient.StartMaintenance(c.ctx, request)
	if err != nil {
		return err
	}

	fmt.Fprintf(tabWriter, "Started draining hosts\n")
	tabWriter.Flush()
	return nil
}

// HostMaintenanceCompleteAction is the action for completing host maintenance. Complete maintenance brings UP a host
// which is in maintenance by posting to /machine/up endpoint of Mesos Master i.e. the machine transitions from DOWN to
// UP state (Please check Mesos Maintenance Primitives for more info)
func (c *Client) HostMaintenanceCompleteAction(hosts string) error {
	hostnames, err := c.ExtractHostnames(hosts, hostSeparator)
	if err != nil {
		return err
	}

	request := &host_svc.CompleteMaintenanceRequest{
		Hostnames: hostnames,
	}
	_, err = c.hostClient.CompleteMaintenance(c.ctx, request)
	if err != nil {
		return err
	}

	fmt.Fprintf(tabWriter, "Maintenance completed\n")
	tabWriter.Flush()
	return nil
}

// HostQueryAction is the action for querying hosts by states. This can be to used to monitor the state of the host(s)
// Eg. When a list of hosts are put into maintenance (`host maintenance start`).
// A host, at any given time, will be in one of the following states
// 		1.HostState_HOST_STATE_UP - The host is up and running
// 		2.HostState_HOST_STATE_DRAINING - The tasks running on the host are being rescheduled and
// 										  there will be no further placement of tasks on the host
//		3.HostState_HOST_STATE_DRAINED - There are no tasks running on this host and it is ready to be 'DOWN'ed
// 		4.HostState_HOST_STATE_DOWN - The host is in maintenance.
func (c *Client) HostQueryAction(states string) error {
	var hostStates []host.HostState
	for _, state := range strings.Split(states, hostSeparator) {
		if state != "" {
			hostStates = append(hostStates, host.HostState(host.HostState_value[state]))
		}
	}

	request := &host_svc.QueryHostsRequest{
		HostStates: hostStates,
	}
	response, err := c.hostClient.QueryHosts(c.ctx, request)
	if err != nil {
		return err
	}

	printHostQueryResponse(response, c.Debug)
	return nil
}

func printHostQueryResponse(r *host_svc.QueryHostsResponse, debug bool) {
	if debug {
		printResponseJSON(r)
	} else {
		if len(r.GetHostInfos()) == 0 {
			fmt.Fprintf(tabWriter, "No hosts found\n")
			return
		}
		fmt.Fprintf(tabWriter, hostQueryFormatHeader)
		for _, h := range r.GetHostInfos() {
			fmt.Fprintf(
				tabWriter,
				hostQueryFormatBody,
				h.GetHostname(),
				h.GetIp(),
				h.GetState(),
			)
		}
	}
	tabWriter.Flush()
}

// HostsGetAction prints all the hosts based on resource requirement
// passed in.
func (c *Client) HostsGetAction(
	cpu float64,
	gpu float64,
	cmpLess bool,
	hosts string) error {
	var hostnames []string
	var err error

	if len(hosts) > 0 {
		hostnames, err = c.ExtractHostnames(hosts, hostSeparator)
		if err != nil {
			return err
		}
	}

	resourceConfig := &pb_task.ResourceConfig{
		CpuLimit: cpu,
		GpuLimit: gpu,
	}

	resp, _ := c.hostMgrClient.GetHostsByQuery(
		c.ctx,
		&hostsvc.GetHostsByQueryRequest{
			Resource:  resourceConfig,
			CmpLess:   cmpLess,
			Hostnames: hostnames,
		})

	printGetHostsResponse(resp)
	return nil
}

func printGetHostsResponse(resp *hostsvc.GetHostsByQueryResponse) {
	defer tabWriter.Flush()

	hosts := resp.GetHosts()
	if len(hosts) == 0 {
		fmt.Fprintln(tabWriter, "No hosts found satisfies the requirement")
	} else {
		sort.Slice(hosts, func(i, j int) bool {
			return strings.Compare(hosts[i].GetHostname(), hosts[j].GetHostname()) < 0
		})

		fmt.Fprint(tabWriter, getHostsFormatHeader)
		for _, host := range hosts {
			resource := scalar.FromMesosResources(host.GetResources())

			fmt.Fprintf(tabWriter,
				getHostsFormatBody,
				host.GetHostname(),
				resource.GetCPU(),
				resource.GetGPU(),
				resource.GetMem(),
				resource.GetDisk(),
				host.GetStatus(),
				getTaskHeldString(host),
			)
		}
	}
}

func getTaskHeldString(host *hostsvc.GetHostsByQueryResponse_Host) string {
	var taskHeldStr string
	for _, taskHeld := range host.GetHeldTasks() {
		taskHeldStr = taskHeldStr + taskHeld.GetValue() + " "
	}

	// remove the last space
	if len(taskHeldStr) != 0 {
		taskHeldStr = taskHeldStr[:len(taskHeldStr)-1]
	}

	return taskHeldStr
}

// DisableKillTasksAction disable the kill task request to mesos master
func (c *Client) DisableKillTasksAction() error {

	_, err := c.hostMgrClient.DisableKillTasks(c.ctx, &hostsvc.DisableKillTasksRequest{})
	if err != nil {
		return err
	}

	fmt.Fprintf(tabWriter, "Disabled kill tasks request\n")
	tabWriter.Flush()
	return nil
}
