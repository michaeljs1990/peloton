import pytest
import grpc
import time
import logging

from peloton_client.pbgen.peloton.api.v0.task import task_pb2
from peloton_client.pbgen.peloton.api.v1alpha.job.stateless import (
    stateless_pb2,
)
from peloton_client.pbgen.peloton.api.v1alpha.job.stateless.stateless_pb2 import (
    JobSpec,
)
from peloton_client.pbgen.peloton.api.v1alpha.pod import pod_pb2

from google.protobuf import json_format

from tests.integration.stateless_job_test.util import (
    assert_pod_id_changed,
    assert_pod_spec_changed,
    assert_pod_spec_equal,
)
from tests.integration.stateless_update import StatelessUpdate
from tests.integration.util import load_test_config
from tests.integration.stateless_job import StatelessJob
from tests.integration.common import IntegrationTestConfig
from tests.integration.stateless_job import INVALID_ENTITY_VERSION_ERR_MESSAGE

pytestmark = [
    pytest.mark.default,
    pytest.mark.stateless,
    pytest.mark.update,
    pytest.mark.random_order(disabled=True),
]

log = logging.getLogger(__name__)

UPDATE_STATELESS_JOB_SPEC = "test_update_stateless_job_spec.yaml"
UPDATE_STATELESS_JOB_ADD_INSTANCES_SPEC = (
    "test_update_stateless_job_add_instances_spec.yaml"
)
UPDATE_STATELESS_JOB_UPDATE_AND_ADD_INSTANCES_SPEC = (
    "test_update_stateless_job_update_and_add_instances_spec.yaml"
)
UPDATE_STATELESS_JOB_UPDATE_REDUCE_INSTANCES_SPEC = (
    "test_stateless_job_spec.yaml"
)
UPDATE_STATELESS_JOB_BAD_SPEC = "test_stateless_job_with_bad_spec.yaml"
UPDATE_STATELESS_JOB_BAD_HEALTH_CHECK_SPEC = (
    "test_stateless_job_failed_health_check_spec.yaml"
)
UPDATE_STATELESS_JOB_WITH_HEALTH_CHECK_SPEC = (
    "test_stateless_job_successful_health_check_spec.yaml"
)
UPDATE_STATELESS_JOB_INVALID_SPEC = "test_stateless_job_spec_invalid.yaml"
UPDATE_STATELESS_JOB_NO_ERR = "test_stateless_job_exit_without_err_spec.yaml"


def test__create_update(stateless_job, in_place):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")
    old_pod_infos = stateless_job.query_pods()
    old_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()
    update = StatelessUpdate(
        stateless_job, updated_job_file=UPDATE_STATELESS_JOB_SPEC
    )
    update.create(in_place=in_place)
    update.wait_for_state(goal_state="SUCCEEDED")
    new_pod_infos = stateless_job.query_pods()
    new_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()
    assert_pod_id_changed(old_pod_infos, new_pod_infos)
    assert_pod_spec_changed(old_instance_zero_spec, new_instance_zero_spec)


def test__create_update_add_instances(stateless_job, in_place):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")
    old_pod_infos = stateless_job.query_pods()
    update = StatelessUpdate(
        stateless_job, updated_job_file=UPDATE_STATELESS_JOB_ADD_INSTANCES_SPEC
    )
    update.create(in_place=in_place)
    update.wait_for_state(goal_state="SUCCEEDED")
    new_pod_infos = stateless_job.query_pods()
    assert len(old_pod_infos) == 3
    assert len(new_pod_infos) == 5


def test__create_update_update_and_add_instances(stateless_job, in_place):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")
    old_pod_infos = stateless_job.query_pods()
    old_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()
    update = StatelessUpdate(
        stateless_job,
        updated_job_file=UPDATE_STATELESS_JOB_UPDATE_AND_ADD_INSTANCES_SPEC,
    )
    update.create(in_place=in_place)
    update.wait_for_state(goal_state="SUCCEEDED")
    new_pod_infos = stateless_job.query_pods()
    new_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()
    assert len(old_pod_infos) == 3
    assert len(new_pod_infos) == 5
    assert_pod_id_changed(old_pod_infos, new_pod_infos)
    assert_pod_spec_changed(old_instance_zero_spec, new_instance_zero_spec)


def test__create_update_update_start_paused(stateless_job, in_place):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")
    old_pod_infos = stateless_job.query_pods()
    old_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()
    update = StatelessUpdate(
        stateless_job,
        updated_job_file=UPDATE_STATELESS_JOB_UPDATE_AND_ADD_INSTANCES_SPEC,
        start_paused=True,
    )
    update.create(in_place=in_place)
    update.wait_for_state(goal_state="PAUSED")
    update.resume()
    update.wait_for_state(goal_state="SUCCEEDED")
    new_pod_infos = stateless_job.query_pods()
    new_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()
    assert len(old_pod_infos) == 3
    assert len(new_pod_infos) == 5
    assert_pod_id_changed(old_pod_infos, new_pod_infos)
    assert_pod_spec_changed(old_instance_zero_spec, new_instance_zero_spec)


def test__create_update_with_batch_size(stateless_job, in_place):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")
    old_pod_infos = stateless_job.query_pods()
    old_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()
    update = StatelessUpdate(
        stateless_job, updated_job_file=UPDATE_STATELESS_JOB_SPEC, batch_size=1
    )
    update.create(in_place=in_place)
    update.wait_for_state(goal_state="SUCCEEDED")
    new_pod_infos = stateless_job.query_pods()
    new_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()
    assert_pod_id_changed(old_pod_infos, new_pod_infos)
    assert_pod_spec_changed(old_instance_zero_spec, new_instance_zero_spec)


def test__create_update_add_instances_with_batch_size(stateless_job, in_place):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")
    old_pod_infos = stateless_job.query_pods()
    update = StatelessUpdate(
        stateless_job,
        updated_job_file=UPDATE_STATELESS_JOB_ADD_INSTANCES_SPEC,
        batch_size=1,
    )
    update.create(in_place=in_place)
    update.wait_for_state(goal_state="SUCCEEDED")
    new_pod_infos = stateless_job.query_pods()
    assert len(old_pod_infos) == 3
    assert len(new_pod_infos) == 5


def test__create_update_update_and_add_instances_with_batch(stateless_job, in_place):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")
    old_pod_infos = stateless_job.query_pods()
    old_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()
    update = StatelessUpdate(
        stateless_job,
        updated_job_file=UPDATE_STATELESS_JOB_UPDATE_AND_ADD_INSTANCES_SPEC,
        batch_size=1,
    )
    update.create(in_place=in_place)
    update.wait_for_state(goal_state="SUCCEEDED")
    new_pod_infos = stateless_job.query_pods()
    new_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()
    assert len(old_pod_infos) == 3
    assert len(new_pod_infos) == 5
    assert_pod_id_changed(old_pod_infos, new_pod_infos)
    assert_pod_spec_changed(old_instance_zero_spec, new_instance_zero_spec)


def test__create_update_update_restart_jobmgr(stateless_job, jobmgr, in_place):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")
    old_pod_infos = stateless_job.query_pods()
    old_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()
    update = StatelessUpdate(
        stateless_job,
        updated_job_file=UPDATE_STATELESS_JOB_UPDATE_AND_ADD_INSTANCES_SPEC,
        batch_size=1,
    )
    update.create(in_place=in_place)
    jobmgr.restart()
    update.wait_for_state(goal_state="SUCCEEDED")
    new_pod_infos = stateless_job.query_pods()
    new_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()
    assert len(old_pod_infos) == 3
    assert len(new_pod_infos) == 5
    assert_pod_id_changed(old_pod_infos, new_pod_infos)
    assert_pod_spec_changed(old_instance_zero_spec, new_instance_zero_spec)


def test__create_update_bad_version(stateless_job, in_place):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")
    update = StatelessUpdate(
        stateless_job,
        updated_job_file=UPDATE_STATELESS_JOB_UPDATE_AND_ADD_INSTANCES_SPEC,
        batch_size=1,
    )
    try:
        update.create(entity_version="1-2-3", in_place=in_place)
    except grpc.RpcError as e:
        assert e.code() == grpc.StatusCode.ABORTED
        assert INVALID_ENTITY_VERSION_ERR_MESSAGE in e.details()
        return
    raise Exception("entity version mismatch error not received")


def test__pause_update_bad_version(stateless_job, in_place):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")
    update = StatelessUpdate(
        stateless_job,
        updated_job_file=UPDATE_STATELESS_JOB_UPDATE_AND_ADD_INSTANCES_SPEC,
        batch_size=1,
    )
    update.create(in_place=in_place)
    try:
        update.pause(entity_version="1-2-3")
    except grpc.RpcError as e:
        assert e.code() == grpc.StatusCode.ABORTED
        assert INVALID_ENTITY_VERSION_ERR_MESSAGE in e.details()
        return
    raise Exception("entity version mismatch error not received")


def test__resume_update_bad_version(stateless_job, in_place):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")
    update = StatelessUpdate(
        stateless_job,
        updated_job_file=UPDATE_STATELESS_JOB_UPDATE_AND_ADD_INSTANCES_SPEC,
        start_paused=True,
        batch_size=1,
    )
    update.create(in_place=in_place)
    try:
        update.resume(entity_version="1-2-3")
    except grpc.RpcError as e:
        assert e.code() == grpc.StatusCode.ABORTED
        assert INVALID_ENTITY_VERSION_ERR_MESSAGE in e.details()
        return
    raise Exception("entity version mismatch error not received")


def test__abort_update_bad_version(stateless_job, in_place):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")
    update = StatelessUpdate(
        stateless_job,
        updated_job_file=UPDATE_STATELESS_JOB_UPDATE_AND_ADD_INSTANCES_SPEC,
        batch_size=1,
    )
    update.create(in_place=in_place)
    try:
        update.abort(entity_version="1-2-3")
    except grpc.RpcError as e:
        assert e.code() == grpc.StatusCode.ABORTED
        assert INVALID_ENTITY_VERSION_ERR_MESSAGE in e.details()
        return
    raise Exception("entity version mismatch error not received")


def test__create_update_stopped_job(stateless_job, in_place):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")
    old_pod_infos = stateless_job.query_pods()
    old_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()

    old_pod_states = set()
    for pod_info in old_pod_infos:
        old_pod_states.add(pod_info.spec.pod_name.value)

    stateless_job.stop()
    stateless_job.wait_for_state(goal_state="KILLED")
    update = StatelessUpdate(
        stateless_job,
        updated_job_file=UPDATE_STATELESS_JOB_UPDATE_AND_ADD_INSTANCES_SPEC,
        batch_size=1,
    )
    update.create(in_place=in_place)
    stateless_job.start()
    update.wait_for_state(goal_state="SUCCEEDED")
    stateless_job.wait_for_state(goal_state="RUNNING")
    new_pod_infos = stateless_job.query_pods()
    new_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()
    assert len(old_pod_infos) == 3
    assert len(new_pod_infos) == 5
    assert_pod_id_changed(old_pod_infos, new_pod_infos)
    assert_pod_spec_changed(old_instance_zero_spec, new_instance_zero_spec)

    # Only new instances should be RUNNING
    for pod_info in new_pod_infos:
        if pod_info.spec.pod_name.value in new_pod_infos:
            assert pod_info.status.state == pod_pb2.POD_STATE_KILLED
        else:
            assert pod_info.status.state == pod_pb2.POD_STATE_RUNNING


def test__create_update_stopped_tasks(stateless_job, in_place):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")
    old_pod_infos = stateless_job.query_pods()
    old_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()
    stateless_job.stop()
    update = StatelessUpdate(
        stateless_job,
        updated_job_file=UPDATE_STATELESS_JOB_UPDATE_AND_ADD_INSTANCES_SPEC,
        batch_size=1,
    )
    update.create(in_place=in_place)
    update.wait_for_state(goal_state="SUCCEEDED")
    stateless_job.wait_for_state(goal_state="KILLED")
    stateless_job.start()
    stateless_job.wait_for_state(goal_state="RUNNING")
    new_pod_infos = stateless_job.query_pods()
    new_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()
    assert len(old_pod_infos) == 3
    assert len(new_pod_infos) == 5
    assert_pod_id_changed(old_pod_infos, new_pod_infos)
    assert_pod_spec_changed(old_instance_zero_spec, new_instance_zero_spec)


def test__create_multiple_consecutive_updates(stateless_job, in_place):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")
    old_pod_infos = stateless_job.query_pods()
    old_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()
    update1 = StatelessUpdate(
        stateless_job, updated_job_file=UPDATE_STATELESS_JOB_ADD_INSTANCES_SPEC
    )
    update1.create(in_place=in_place)
    update2 = StatelessUpdate(
        stateless_job,
        updated_job_file=UPDATE_STATELESS_JOB_UPDATE_AND_ADD_INSTANCES_SPEC,
        batch_size=1,
    )
    update2.create(in_place=in_place)
    update2.wait_for_state(goal_state="SUCCEEDED")
    new_pod_infos = stateless_job.query_pods()
    new_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()
    assert len(old_pod_infos) == 3
    assert len(new_pod_infos) == 5
    assert_pod_id_changed(old_pod_infos, new_pod_infos)
    assert_pod_spec_changed(old_instance_zero_spec, new_instance_zero_spec)


def test__abort_update(stateless_job, in_place):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")
    update = StatelessUpdate(
        stateless_job,
        updated_job_file=UPDATE_STATELESS_JOB_UPDATE_AND_ADD_INSTANCES_SPEC,
        batch_size=1,
    )
    update.create(in_place=in_place)
    update.wait_for_state(goal_state="ROLLING_FORWARD")
    update.abort()
    update.wait_for_state(goal_state="ABORTED")


def test__update_reduce_instances(stateless_job, in_place):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")
    old_pod_infos = stateless_job.query_pods()
    assert len(old_pod_infos) == 3
    # first increase instances
    update = StatelessUpdate(
        stateless_job, updated_job_file=UPDATE_STATELESS_JOB_ADD_INSTANCES_SPEC
    )
    update.create()
    update.wait_for_state(goal_state="SUCCEEDED")
    new_pod_infos = stateless_job.query_pods()
    assert len(new_pod_infos) == 5
    # now reduce instances
    update = StatelessUpdate(
        stateless_job,
        updated_job_file=UPDATE_STATELESS_JOB_UPDATE_REDUCE_INSTANCES_SPEC,
    )
    update.create(in_place=in_place)
    update.wait_for_state(goal_state="SUCCEEDED")
    new_pod_infos = stateless_job.query_pods()
    assert len(new_pod_infos) == 3
    # now increase back again
    update = StatelessUpdate(
        stateless_job, updated_job_file=UPDATE_STATELESS_JOB_ADD_INSTANCES_SPEC
    )
    update.create()
    update.wait_for_state(goal_state="SUCCEEDED")
    new_pod_infos = stateless_job.query_pods()
    assert len(new_pod_infos) == 5


def test__update_reduce_instances_stopped_tasks(stateless_job, in_place):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")
    old_pod_infos = stateless_job.query_pods()
    assert len(old_pod_infos) == 3
    # first increase instances
    update = StatelessUpdate(
        stateless_job, updated_job_file=UPDATE_STATELESS_JOB_ADD_INSTANCES_SPEC
    )
    update.create(in_place=in_place)
    update.wait_for_state(goal_state="SUCCEEDED")
    new_pod_infos = stateless_job.query_pods()
    assert len(new_pod_infos) == 5
    # now stop last 2 tasks
    ranges = task_pb2.InstanceRange(to=5)
    setattr(ranges, "from", 3)
    stateless_job.stop(ranges=[ranges])
    # now reduce instance count
    update = StatelessUpdate(
        stateless_job,
        updated_job_file=UPDATE_STATELESS_JOB_UPDATE_REDUCE_INSTANCES_SPEC,
    )
    update.create(in_place=in_place)
    update.wait_for_state(goal_state="SUCCEEDED")
    new_pod_infos = stateless_job.query_pods()
    assert len(new_pod_infos) == 3


# test__create_update_bad_config tests creating an update with bad config
# without rollback
def test__create_update_with_bad_config(stateless_job, in_place):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")
    old_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()

    update = StatelessUpdate(
        stateless_job,
        updated_job_file=UPDATE_STATELESS_JOB_BAD_SPEC,
        max_failure_instances=3,
        max_instance_attempts=1,
    )
    update.create(in_place=in_place)
    update.wait_for_state(goal_state="FAILED", failed_state="SUCCEEDED")
    new_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()

    assert_pod_spec_changed(old_instance_zero_spec, new_instance_zero_spec)
    for pod_info in stateless_job.query_pods():
        assert pod_info.status.state == pod_pb2.POD_STATE_FAILED


# test__create_update_add_instances_with_bad_config
# tests creating an update with bad config and more instances
# without rollback
def test__create_update_add_instances_with_bad_config(stateless_job, in_place):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")

    job_spec_dump = load_test_config(UPDATE_STATELESS_JOB_BAD_SPEC)
    updated_job_spec = JobSpec()
    json_format.ParseDict(job_spec_dump, updated_job_spec)

    updated_job_spec.instance_count = stateless_job.job_spec.instance_count + 3

    update = StatelessUpdate(
        stateless_job,
        batch_size=1,
        updated_job_spec=updated_job_spec,
        max_failure_instances=1,
        max_instance_attempts=1,
    )
    update.create(in_place=in_place)
    update.wait_for_state(goal_state="FAILED", failed_state="SUCCEEDED")

    # only one instance should be added
    assert (
        len(stateless_job.query_pods())
        == stateless_job.job_spec.instance_count + 1
    )


# test__create_update_reduce_instances_with_bad_config
# tests creating an update with bad config and fewer instances
# without rollback
def test__create_update_reduce_instances_with_bad_config(stateless_job, in_place):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")
    old_pod_infos = stateless_job.query_pods()

    job_spec_dump = load_test_config(UPDATE_STATELESS_JOB_BAD_SPEC)
    updated_job_spec = JobSpec()
    json_format.ParseDict(job_spec_dump, updated_job_spec)

    updated_job_spec.instance_count = stateless_job.job_spec.instance_count - 1

    update = StatelessUpdate(
        stateless_job,
        updated_job_spec=updated_job_spec,
        batch_size=1,
        max_failure_instances=1,
        max_instance_attempts=1,
    )
    update.create(in_place=in_place)
    update.wait_for_state(goal_state="FAILED", failed_state="SUCCEEDED")
    new_pod_infos = stateless_job.query_pods()
    assert len(old_pod_infos) == len(new_pod_infos)


# test__create_update_with_failed_health_check
# tests an update fails even if the new task state is RUNNING,
# as long as the health check fails
def test__create_update_with_failed_health_check(stateless_job, in_place):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")

    update = StatelessUpdate(
        stateless_job,
        updated_job_file=UPDATE_STATELESS_JOB_BAD_HEALTH_CHECK_SPEC,
        max_failure_instances=1,
        max_instance_attempts=1,
    )
    update.create(in_place=in_place)
    update.wait_for_state(goal_state="FAILED", failed_state="SUCCEEDED")


# test__create_update_to_disable_health_check tests an update which
# disables healthCheck
def test__create_update_to_disable_health_check(in_place):
    job = StatelessJob(
        job_file=UPDATE_STATELESS_JOB_WITH_HEALTH_CHECK_SPEC,
        config=IntegrationTestConfig(max_retry_attempts=100),
    )
    job.create()
    job.wait_for_state(goal_state="RUNNING")

    job.job_spec.default_spec.containers[0].liveness_check.enabled = False
    update = StatelessUpdate(
        job,
        updated_job_spec=job.job_spec,
        max_failure_instances=1,
        max_instance_attempts=1,
    )
    update.create(in_place=in_place)
    update.wait_for_state(goal_state="SUCCEEDED")


# test__create_update_to_enable_health_check tests an update which
# enables healthCheck
def test__create_update_to_enable_health_check(in_place):
    job = StatelessJob(
        job_file=UPDATE_STATELESS_JOB_WITH_HEALTH_CHECK_SPEC,
        config=IntegrationTestConfig(max_retry_attempts=100),
    )
    job.job_spec.default_spec.containers[0].liveness_check.enabled = False
    job.create()
    job.wait_for_state(goal_state="RUNNING")

    job.job_spec.default_spec.containers[0].liveness_check.enabled = True
    update = StatelessUpdate(
        job,
        updated_job_spec=job.job_spec,
        max_failure_instances=1,
        max_instance_attempts=1,
    )
    update.create(in_place=in_place)
    update.wait_for_state(goal_state="SUCCEEDED")


# test__create_update_to_unset_health_check tests an update to unset
# health check config
def test__create_update_to_unset_health_check(in_place):
    job = StatelessJob(
        job_file=UPDATE_STATELESS_JOB_WITH_HEALTH_CHECK_SPEC,
        config=IntegrationTestConfig(max_retry_attempts=100),
    )
    job.create()
    job.wait_for_state(goal_state="RUNNING")

    update = StatelessUpdate(
        job,
        updated_job_file=UPDATE_STATELESS_JOB_SPEC,
        max_failure_instances=1,
        max_instance_attempts=1,
    )
    update.create(in_place=in_place)
    update.wait_for_state(goal_state="SUCCEEDED")


# test__create_update_to_unset_health_check tests an update to set
# health check config for a job without health check set
def test__create_update_to_set_health_check(in_place):
    job = StatelessJob(
        job_file=UPDATE_STATELESS_JOB_SPEC,
        config=IntegrationTestConfig(max_retry_attempts=100),
    )
    job.create()
    job.wait_for_state(goal_state="RUNNING")

    update = StatelessUpdate(
        job,
        updated_job_file=UPDATE_STATELESS_JOB_WITH_HEALTH_CHECK_SPEC,
        max_failure_instances=1,
        max_instance_attempts=1,
    )
    update.create(in_place=in_place)
    update.wait_for_state(goal_state="SUCCEEDED")


# test__create_update_to_change_health_check_config tests an update which
# changes healthCheck
def test__create_update_to_change_health_check_config(in_place):
    job = StatelessJob(
        job_file=UPDATE_STATELESS_JOB_WITH_HEALTH_CHECK_SPEC,
        config=IntegrationTestConfig(max_retry_attempts=100),
    )
    job.job_spec.default_spec.containers[0].liveness_check.enabled = False
    job.create()
    job.wait_for_state(goal_state="RUNNING")

    job.job_spec.default_spec.containers[
        0
    ].liveness_check.initial_interval_secs = 2
    update = StatelessUpdate(
        job,
        updated_job_spec=job.job_spec,
        max_failure_instances=1,
        max_instance_attempts=1,
    )
    update.create(in_place=in_place)
    update.wait_for_state(goal_state="SUCCEEDED")


# test__auto_rollback_update_with_bad_config tests creating an update with bad config
# with rollback
def test__auto_rollback_update_with_bad_config(stateless_job, in_place):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")
    old_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()

    update = StatelessUpdate(
        stateless_job,
        updated_job_file=UPDATE_STATELESS_JOB_BAD_SPEC,
        roll_back_on_failure=True,
        max_failure_instances=1,
        max_instance_attempts=1,
    )
    update.create(in_place=in_place)
    update.wait_for_state(goal_state="ROLLED_BACK")
    new_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()

    assert_pod_spec_equal(old_instance_zero_spec, new_instance_zero_spec)


# test__auto_rollback_update_add_instances_with_bad_config
# tests creating an update with bad config and more instances
# with rollback
def test__auto_rollback_update_add_instances_with_bad_config(stateless_job, in_place):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")
    old_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()

    job_spec_dump = load_test_config(UPDATE_STATELESS_JOB_BAD_SPEC)
    updated_job_spec = JobSpec()
    json_format.ParseDict(job_spec_dump, updated_job_spec)

    updated_job_spec.instance_count = stateless_job.job_spec.instance_count + 3

    update = StatelessUpdate(
        stateless_job,
        updated_job_spec=updated_job_spec,
        roll_back_on_failure=True,
        max_failure_instances=1,
        max_instance_attempts=1,
    )
    update.create(in_place=in_place)
    update.wait_for_state(goal_state="ROLLED_BACK")
    new_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()

    # no instance should be added
    assert (
        len(stateless_job.query_pods())
        == stateless_job.job_spec.instance_count
    )
    assert_pod_spec_equal(old_instance_zero_spec, new_instance_zero_spec)


# test__auto_rollback_update_reduce_instances_with_bad_config
# tests creating an update with bad config and fewer instances
# with rollback
def test__auto_rollback_update_reduce_instances_with_bad_config(stateless_job, in_place):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")
    old_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()

    job_spec_dump = load_test_config(UPDATE_STATELESS_JOB_BAD_SPEC)
    updated_job_spec = JobSpec()
    json_format.ParseDict(job_spec_dump, updated_job_spec)

    updated_job_spec.instance_count = stateless_job.job_spec.instance_count - 1

    update = StatelessUpdate(
        stateless_job,
        updated_job_spec=updated_job_spec,
        roll_back_on_failure=True,
        max_failure_instances=1,
        max_instance_attempts=1,
    )
    update.create(in_place=in_place)
    update.wait_for_state(goal_state="ROLLED_BACK")
    new_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()

    # no instance should be removed
    assert (
        len(stateless_job.query_pods())
        == stateless_job.job_spec.instance_count
    )
    assert_pod_spec_equal(old_instance_zero_spec, new_instance_zero_spec)


# test__auto_rollback_update_with_failed_health_check
# tests an update fails even if the new task state is RUNNING,
# as long as the health check fails
def test__auto_rollback_update_with_failed_health_check(stateless_job, in_place):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")
    old_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()

    update = StatelessUpdate(
        stateless_job,
        updated_job_file=UPDATE_STATELESS_JOB_BAD_HEALTH_CHECK_SPEC,
        roll_back_on_failure=True,
        max_failure_instances=1,
        max_instance_attempts=1,
    )
    update.create(in_place=in_place)
    update.wait_for_state(goal_state="ROLLED_BACK")
    new_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()
    assert_pod_spec_equal(old_instance_zero_spec, new_instance_zero_spec)


# test__pause_resume_initialized_update test pause and resume
#  an update in initialized state
def test__pause_resume_initialized_update(stateless_job, in_place):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")
    old_pod_infos = stateless_job.query_pods()
    old_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()
    update = StatelessUpdate(
        stateless_job, batch_size=1, updated_job_file=UPDATE_STATELESS_JOB_SPEC
    )
    update.create(in_place=in_place)
    # immediately pause the update, so the update may still be INITIALIZED
    update.pause()
    update.wait_for_state(goal_state="PAUSED")
    update.resume()
    update.wait_for_state(goal_state="SUCCEEDED")
    new_pod_infos = stateless_job.query_pods()
    new_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()
    assert_pod_id_changed(old_pod_infos, new_pod_infos)
    assert_pod_spec_changed(old_instance_zero_spec, new_instance_zero_spec)


# test__pause_resume_initialized_update test pause and resume an update
def test__pause_resume__update(stateless_job, in_place):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")
    old_pod_infos = stateless_job.query_pods()
    old_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()
    update = StatelessUpdate(
        stateless_job, batch_size=1, updated_job_file=UPDATE_STATELESS_JOB_SPEC
    )
    update.create(in_place=in_place)
    # sleep for 1 sec so update can begin to roll forward
    time.sleep(1)
    update.pause()
    update.wait_for_state(goal_state="PAUSED")
    update.resume()
    update.wait_for_state(goal_state="SUCCEEDED")
    new_pod_infos = stateless_job.query_pods()
    new_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()
    assert_pod_id_changed(old_pod_infos, new_pod_infos)
    assert_pod_spec_changed(old_instance_zero_spec, new_instance_zero_spec)


# test_manual_rollback manually rolls back a running update when
# the instance count is reduced in the rollback.
# Note that manual rollback in peloton is just updating to the
# previous job configuration
def test_manual_rollback_reduce_instances(stateless_job, in_place):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")
    old_pod_infos = stateless_job.query_pods()
    old_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()
    update = StatelessUpdate(
        stateless_job, updated_job_file=UPDATE_STATELESS_JOB_ADD_INSTANCES_SPEC
    )
    update.create(in_place=in_place)
    # manually rollback the update
    update2 = StatelessUpdate(
        stateless_job,
        updated_job_file=UPDATE_STATELESS_JOB_UPDATE_REDUCE_INSTANCES_SPEC,
    )
    update2.create(in_place=in_place)
    update2.wait_for_state(goal_state="SUCCEEDED")
    new_pod_infos = stateless_job.query_pods()
    assert len(old_pod_infos) == len(new_pod_infos)
    new_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()
    assert_pod_spec_equal(old_instance_zero_spec, new_instance_zero_spec)


# test_manual_rollback manually rolls back a running update when
# the instance count is increased in the rollback
def test_manual_rollback_increase_instances(stateless_job, in_place):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")
    update = StatelessUpdate(
        stateless_job, updated_job_file=UPDATE_STATELESS_JOB_ADD_INSTANCES_SPEC
    )
    update.create(in_place=in_place)
    update.wait_for_state(goal_state="SUCCEEDED")
    old_pod_infos = stateless_job.query_pods()
    old_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()
    # reduce instance count and then roll it back
    update2 = StatelessUpdate(
        stateless_job,
        updated_job_file=UPDATE_STATELESS_JOB_UPDATE_REDUCE_INSTANCES_SPEC,
    )
    update2.create(in_place=in_place)
    update3 = StatelessUpdate(
        stateless_job, updated_job_file=UPDATE_STATELESS_JOB_ADD_INSTANCES_SPEC
    )
    update3.create(in_place=in_place)
    update3.wait_for_state(goal_state="SUCCEEDED")
    new_pod_infos = stateless_job.query_pods()
    assert len(old_pod_infos) == len(new_pod_infos)
    new_instance_zero_spec = stateless_job.get_pod(0).get_pod_spec()
    assert_pod_spec_equal(old_instance_zero_spec, new_instance_zero_spec)


# test_auto_rollback_reduce_instances
#  rolls back a failed update when
# the instance count is reduced in the rollback.
def test_auto_rollback_reduce_instances(stateless_job, in_place):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")

    job_spec_dump = load_test_config(
        UPDATE_STATELESS_JOB_BAD_HEALTH_CHECK_SPEC
    )
    updated_job_spec = JobSpec()
    json_format.ParseDict(job_spec_dump, updated_job_spec)

    # increase the instance count
    updated_job_spec.instance_count = stateless_job.job_spec.instance_count + 3

    update = StatelessUpdate(
        stateless_job,
        updated_job_spec=updated_job_spec,
        roll_back_on_failure=True,
        max_instance_attempts=1,
        max_failure_instances=1,
        batch_size=1,
    )
    update.create(in_place=in_place)
    update.wait_for_state(goal_state="ROLLED_BACK")
    assert (
        len(stateless_job.query_pods())
        == stateless_job.job_spec.instance_count
    )


# test_update_create_failure_invalid_spec tests the
# update create failure due to invalid spec in request
def test_update_create_failure_invalid_spec(stateless_job, in_place):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")

    update = StatelessUpdate(
        stateless_job, updated_job_file=UPDATE_STATELESS_JOB_INVALID_SPEC
    )
    try:
        update.create(in_place=in_place)
    except grpc.RpcError as e:
        assert e.code() == grpc.StatusCode.INVALID_ARGUMENT
        return
    raise Exception("job spec validation error not received")


# test_update_killed_job tests updating a killed job.
# The job should be updated but still remain in killed state
def test_update_killed_job(in_place):
    job = StatelessJob(job_file=UPDATE_STATELESS_JOB_ADD_INSTANCES_SPEC)
    job.create()
    job.wait_for_state(goal_state="RUNNING")

    job.stop()
    job.wait_for_state(goal_state="KILLED")

    update = StatelessUpdate(
        job, updated_job_file=UPDATE_STATELESS_JOB_UPDATE_REDUCE_INSTANCES_SPEC
    )
    update.create(in_place=in_place)
    update.wait_for_state(goal_state="SUCCEEDED")

    assert job.get_spec().instance_count == 3
    assert job.get_status().state == stateless_pb2.JOB_STATE_KILLED


# test_start_job_with_active_update tests
# starting a job with an active update
def test_start_job_with_active_update(stateless_job, in_place):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")
    assert len(stateless_job.query_pods()) == 3
    stateless_job.stop()

    update = StatelessUpdate(
        stateless_job,
        updated_job_file=UPDATE_STATELESS_JOB_UPDATE_AND_ADD_INSTANCES_SPEC,
        batch_size=1,
    )

    update.create(in_place=in_place)
    stateless_job.start()

    update.wait_for_state(goal_state="SUCCEEDED")
    stateless_job.wait_for_all_pods_running()
    assert len(stateless_job.query_pods()) == 5


# test_stop_running_job_with_active_update_add_instances tests
# stopping a running job with an active update(add instances)
def test_stop_running_job_with_active_update_add_instances(stateless_job, in_place):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")
    assert len(stateless_job.query_pods()) == 3

    update = StatelessUpdate(
        stateless_job,
        updated_job_file=UPDATE_STATELESS_JOB_UPDATE_AND_ADD_INSTANCES_SPEC,
        batch_size=1,
    )
    update.create(in_place=in_place)
    update.wait_for_state(goal_state="ROLLING_FORWARD")

    stateless_job.stop()
    update.wait_for_state(goal_state="SUCCEEDED")
    assert stateless_job.get_spec().instance_count == 5


# test_stop_running_job_with_active_update_remove_instances tests
# stopping a running job with an active update(remove instances)
def test_stop_running_job_with_active_update_remove_instances(in_place):
    stateless_job = StatelessJob(
        job_file=UPDATE_STATELESS_JOB_ADD_INSTANCES_SPEC
    )
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")
    assert len(stateless_job.query_pods()) == 5

    update = StatelessUpdate(
        stateless_job,
        updated_job_file=UPDATE_STATELESS_JOB_UPDATE_REDUCE_INSTANCES_SPEC,
        batch_size=1,
    )
    update.create(in_place=in_place)
    update.wait_for_state(goal_state="ROLLING_FORWARD")

    stateless_job.stop()
    update.wait_for_state(goal_state="SUCCEEDED")
    assert stateless_job.get_spec().instance_count == 3


# test_stop_running_job_with_active_update_same_instance_count tests stopping
# a running job with an active update that doesn't change instance count
def test_stop_running_job_with_active_update_same_instance_count(
    stateless_job,
    in_place
):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state="RUNNING")

    stateless_job.job_spec.default_spec.containers[
        0
    ].command.value = "sleep 100"
    update = StatelessUpdate(
        stateless_job,
        updated_job_spec=stateless_job.job_spec,
        max_failure_instances=1,
        max_instance_attempts=1,
    )
    update.create(in_place=in_place)
    stateless_job.stop()
    update.wait_for_state(goal_state="SUCCEEDED")
    assert stateless_job.get_spec().instance_count == 3
    assert (
        stateless_job.get_spec().default_spec.containers[0].command.value
        == "sleep 100"
    )


# test__create_update_before_job_fully_created creates an update
# right after a job is created. It tests the case that job can be
# updated before it is fully created
def test__create_update_before_job_fully_created(stateless_job, in_place):
    stateless_job.create()
    update = StatelessUpdate(
        stateless_job, updated_job_file=UPDATE_STATELESS_JOB_SPEC
    )
    update.create(in_place=in_place)
    update.wait_for_state(goal_state="SUCCEEDED")
    assert (
        stateless_job.get_spec().default_spec.containers[0].command.value
        == "while :; do echo updated; sleep 10; done"
    )


# test__in_place_update_success_rate tests that in-place update
# should succeed when every daemon is in healthy state.
# It starts a job with 30 instances, and start the in-place update
# without batch size, then it tests if any pod is running on unexpected
# host.
def test__in_place_update_success_rate(stateless_job):
    stateless_job.job_spec.instance_count = 30
    stateless_job.create()
    stateless_job.wait_for_all_pods_running()
    old_pod_infos = stateless_job.query_pods()

    job_spec_dump = load_test_config(UPDATE_STATELESS_JOB_SPEC)
    updated_job_spec = JobSpec()
    json_format.ParseDict(job_spec_dump, updated_job_spec)

    updated_job_spec.instance_count = 30
    update = StatelessUpdate(stateless_job,
                             updated_job_spec=updated_job_spec,
                             batch_size=0)
    update.create(in_place=True)
    update.wait_for_state(goal_state='SUCCEEDED')

    new_pod_infos = stateless_job.query_pods()

    old_pod_dict = {}
    new_pod_dict = {}

    for old_pod_info in old_pod_infos:
        split_index = old_pod_info.status.pod_id.value.rfind('-')
        pod_name = old_pod_info.status.pod_id.value[:split_index]
        old_pod_dict[pod_name] = old_pod_info.status.host

    for new_pod_info in new_pod_infos:
        split_index = new_pod_info.status.pod_id.value.rfind('-')
        pod_name = new_pod_info.status.pod_id.value[:split_index]
        new_pod_dict[pod_name] = new_pod_info.status.host

    count = 0
    for pod_name, pod_id in old_pod_dict.items():
        if new_pod_dict[pod_name] != old_pod_dict[pod_name]:
            log.info("%s, prev:%s, cur:%s", pod_name,
                     old_pod_dict[pod_name], new_pod_dict[pod_name])
            count = count + 1
    log.info("total mismatch: %d", count)
    assert count == 0
