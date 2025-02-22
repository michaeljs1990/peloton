import logging
import pytest
import time

from tests.integration.host import (
    query_hosts,
    start_maintenance,
    complete_maintenance,
    wait_for_host_state,
    is_host_in_state,
    draining_period_sec,
)
from tests.integration.conftest import Container

from peloton_client.pbgen.peloton.api.v0.host import host_pb2 as hpb
from peloton_client.pbgen.peloton.api.v0.task import task_pb2 as task

pytestmark = [
    pytest.mark.default,
    pytest.mark.preemption,
    pytest.mark.random_order(disabled=True),
]

log = logging.getLogger(__name__)


@pytest.fixture
def maintenance(request):
    draining_hosts = []

    def start(hosts):
        resp = start_maintenance(hosts)
        if not resp:
            log.error("Start maintenance failed:" + resp)
            return resp

        draining_hosts.extend(hosts)
        return resp

    def stop(hosts):
        resp = complete_maintenance(hosts)
        if not resp:
            log.error("Complete maintenance failed:" + resp)
            return resp

        for h in hosts:
            draining_hosts.remove(h)
            # The mesos-agent containers needs to be started explicitly as they would
            # have been stopped when the agents transition to DOWN
            Container([h]).start()

        return resp

    def clean_up():
        if not draining_hosts:
            return
        for h in draining_hosts:
            wait_for_host_state(h, hpb.HOST_STATE_DOWN)
        stop(draining_hosts)

    request.addfinalizer(clean_up)

    response = dict()
    response["start"] = start
    response["stop"] = stop
    return response


def get_host_in_state(state):
    """
    returns a host in the specified state. Note that the caller should make sure
    there is at least one host in the the requested state.
    :param state: host_pb2.HostState
    :return: Hostname of a host in the specified state
    """
    resp = query_hosts([state])
    assert len(resp.host_infos) > 0
    return resp.host_infos[0].hostname


# Tests task kill due to host maintenance with the following scenario
# 1. Create a job (with 4 instances) with host affinity constraint (say host A)
#       All 4 instances should transition to RUNNING
# 2. Start Peloton host maintenance on the host A:
#       The host draining kicks in and the tasks on host A should be killed in
#       the next host draining cycle. The tasks should transition to PENDING as
#       host A is DRAINING and there should be no further scheduling on it.
def test__start_maintenance_kill_tasks(host_affinity_job, maintenance):
    # Pick a host that is UP and start maintenance on it
    test_host = get_host_in_state(hpb.HOST_STATE_UP)

    # Set host affinity of the job to the selected host
    host_affinity_job.job_config.defaultConfig.constraint.labelConstraint.label.value = (
        test_host
    )

    host_affinity_job.create()
    host_affinity_job.wait_for_state(goal_state="RUNNING")

    def all_running():
        return all(
            t.state == task.RUNNING
            for t in host_affinity_job.get_tasks().values()
        )

    host_affinity_job.wait_for_condition(all_running)

    constraint = host_affinity_job.job_config.defaultConfig.constraint
    test_host = constraint.labelConstraint.label.value
    resp = maintenance["start"]([test_host])
    assert resp

    def all_pending():
        return all(
            t.state == task.PENDING
            for t in host_affinity_job.get_tasks().values()
        )

    # Wait for tasks to be killed and restarted
    host_affinity_job.wait_for_condition(all_pending)


# Tests a typical host lifecycle. The scenario is as follows
# 1. Select a host in UP state.
# 2. Start Peloton host maintenance on host A:
#       a. Host A should immediately transition to DRAINING.
#       b. Host A should transition to DOWN, latest in the next host draining
#          cycle.
# 3. Complete Maintenance on host A:
#       Host A should not longer be DOWN. It should transition to UP
def test__host_maintenance_lifecycle(host_affinity_job, maintenance):
    # Pick a host that is UP and start maintenance on it
    test_host = get_host_in_state(hpb.HOST_STATE_UP)

    # Set host affinity of the job to the selected host
    host_affinity_job.job_config.defaultConfig.constraint.labelConstraint.label.value = (
        test_host
    )

    host_affinity_job.create()

    # Start maintenance on the selected host
    resp = maintenance["start"]([test_host])
    assert resp

    assert is_host_in_state(test_host, hpb.HOST_STATE_DRAINING)

    # Wait for host to transition to DOWN
    wait_for_host_state(test_host, hpb.HOST_STATE_DOWN)

    # Complete maintenance on the test hosts
    resp = maintenance["stop"]([test_host])
    assert resp

    # Host should no longer be DOWN
    assert not is_host_in_state(test_host, hpb.HOST_STATE_DOWN)

    wait_for_host_state(test_host, hpb.HOST_STATE_UP)


# Tests the resumption of draining process on resmgr recovery. The scenario is
# as follows:
# 1. Select a host in UP state:
# 2. Start Peloton host maintenance on host A.
# 3. Restart resmgr: Before restarting resmgr, jobmgr is stopped to ensure
#                    preemption queue is not polled. On resmgr recovery, the
#                    draining process should resume and host should transition
#                    to DOWN
@pytest.mark.skip(reason="flaky integration test")
def test__host_draining_resumes_on_resmgr_recovery(
    host_affinity_job, maintenance, jobmgr, resmgr
):
    # Pick a host that is UP and start maintenance on it
    test_host = get_host_in_state(hpb.HOST_STATE_UP)

    # Set host affinity of the job to the selected host
    host_affinity_job.job_config.defaultConfig.constraint.labelConstraint.label.value = (
        test_host
    )

    host_affinity_job.create()
    host_affinity_job.wait_for_state(goal_state="RUNNING")

    def all_running():
        return all(
            t.state == task.RUNNING
            for t in host_affinity_job.get_tasks().values()
        )

    host_affinity_job.wait_for_condition(all_running)

    constraint = host_affinity_job.job_config.defaultConfig.constraint
    test_host = constraint.labelConstraint.label.value
    resp = maintenance["start"]([test_host])
    assert resp

    # Stop jobmgr to ensure tasks are not killed
    jobmgr.stop()
    # Sleep for one draining period to ensure maintenance queue is polled
    time.sleep(draining_period_sec)
    resmgr.restart()
    jobmgr.start()

    # Wait for host to transition to DOWN
    wait_for_host_state(test_host, hpb.HOST_STATE_DOWN)


# Tests the resumption of draining process on resmgr recovery. The scenario is
# as follows:
# 1. Select a host in UP state:
# 2. Start Peloton host maintenance on host A.
# 3. Restart hostmgr: Before restarting hostmgr, resmgr is stopped to ensure
#                    maintenance queue is not polled. On hostmgr recovery, the
#                    draining process should resume and host should transition
#                    to DOWN
@pytest.mark.skip(reason="flaky integration test")
def test__host_draining_resumes_on_hostmgr_recovery(
    host_affinity_job, maintenance, resmgr, hostmgr
):
    # Pick a host that is UP and start maintenance on it
    test_host = get_host_in_state(hpb.HOST_STATE_UP)

    # Set host affinity of the job to the selected host
    host_affinity_job.job_config.defaultConfig.constraint.labelConstraint.label.value = (
        test_host
    )

    host_affinity_job.create()
    host_affinity_job.wait_for_state(goal_state="RUNNING")

    def all_running():
        return all(
            t.state == task.RUNNING
            for t in host_affinity_job.get_tasks().values()
        )

    host_affinity_job.wait_for_condition(all_running)
    constraint = host_affinity_job.job_config.defaultConfig.constraint
    test_host = constraint.labelConstraint.label.value

    # Stop resmgr to ensure maintenance queue is not polled
    resmgr.stop()

    resp = maintenance["start"]([test_host])
    assert resp

    hostmgr.restart()
    resmgr.start()

    # Wait for host to transition to DOWN
    wait_for_host_state(test_host, hpb.HOST_STATE_DOWN)
