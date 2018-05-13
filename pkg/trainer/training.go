// Copyright 2018 The Kubeflow Authors
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

// Package trainer is to manage TensorFlow training jobs.
package trainer

import (
	"fmt"
	"io/ioutil"
	"reflect"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"k8s.io/api/core/v1"
	"k8s.io/api/policy/v1beta1"
	k8s_errors "k8s.io/apimachinery/pkg/api/errors"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/kubernetes/pkg/api"

	"github.com/kubeflow/tf-operator/pkg/apis/tensorflow/helper"
	tfv1alpha1 "github.com/kubeflow/tf-operator/pkg/apis/tensorflow/v1alpha1"
	"github.com/kubeflow/tf-operator/pkg/apis/tensorflow/validation"
	tfjobclient "github.com/kubeflow/tf-operator/pkg/client/clientset/versioned"
	"github.com/kubeflow/tf-operator/pkg/client/clientset/versioned/scheme"
	"github.com/kubeflow/tf-operator/pkg/util"
)

// TODO(jlewi): We should switch a New pattern and make trainingJob private so we can
// TrainingJob represents a training job specification.
// ensure correctness on creation.
type TrainingJob struct {
	job *tfv1alpha1.TFJob

	KubeCli kubernetes.Interface

	recorder record.EventRecorder

	Replicas []*TFReplicaSet

	tfJobClient tfjobclient.Interface

	// in memory state of the job.
	// status is the source of truth after job struct is materialized. Changes to the status to be persisted
	// should be made here.
	status tfv1alpha1.TFJobStatus

	memberCounter int

	pdb *v1beta1.PodDisruptionBudget

	// contextLogger is a logger to use for logging information about this replica.
	contextLogger *log.Entry

	placementPlan map[string]int

	PSPlace string

	currentReplicas int

	Worker0Name string
}

// ClusterSpec represents a cluster TensorFlow specification.
// https://www.tensorflow.org/deploy/distributed#create_a_tftrainclusterspec_to_describe_the_cluster
// It is a map from job names to network addresses.
type ClusterSpec map[string][]string

// TaskSpec represents a Task specification.
type TaskSpec struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

//initJob initiate a training job and returns the job specifications.
func initJob(kubeCli kubernetes.Interface, tfJobClient tfjobclient.Interface, recorder record.EventRecorder, job *tfv1alpha1.TFJob) (*TrainingJob, error) {
	j := &TrainingJob{
		KubeCli:     kubeCli,
		tfJobClient: tfJobClient,
		recorder:    recorder,
		Replicas:    make([]*TFReplicaSet, 0),
		job:         job,
		status:      *job.Status.DeepCopy(),

		contextLogger: log.WithFields(log.Fields{
			// We use job to match the key used in controller.go
			// In controller.go we log the key used with the workqueue.
			"job": job.ObjectMeta.Namespace + "/" + job.ObjectMeta.Name,
			"uid": job.ObjectMeta.UID,
		}),
	}

	return j, nil
}

// NewJob method invokes the initJob and check for error
func NewJob(kubeCli kubernetes.Interface, tfJobClient tfjobclient.Interface, recorder record.EventRecorder, job *tfv1alpha1.TFJob, config *tfv1alpha1.ControllerConfig) (*TrainingJob, error) {
	j, err := initJob(kubeCli, tfJobClient, recorder, job)
	if err != nil {
		return nil, err
	}

	return j, nil
}

/*** Jack Lin***/
func (j *TrainingJob) GetJobReplicasSetList() []*TFReplicaSet {
	//log.Info("in GetJobReplicasSetList")
	//log.Info("TrainingJob name: %v", j.job.ObjectMeta.Name)
	//log.Info("j.Replicas: %v", j.Replicas)

	return j.Replicas
}

func (j *TrainingJob) GetJob() *tfv1alpha1.TFJob {
	return j.job
}

func (j *TrainingJob) GetJobPodListStatus() (total, running, pending int, err error) {
	//var totalTmp, runningTmp, pendingTmp int = 0
	for _, r := range j.Replicas {
		if r.Spec.TFReplicaType == tfv1alpha1.WORKER {
			totalTmp := int(*r.Spec.Replicas)
			runningTmp, pendingTmp, err := r.GetPodStatus()
			if err != nil {
				return 0, 0, 0, err
			}

			total += totalTmp
			running += runningTmp
			pending += pendingTmp
		}

	}
	return

}

/*** Jack Lin***/

// Update replaces the TFJob corresponding to TrainingJob with the provided job.
// This function is used when the Spec/Status of the job is modified outside the controller.
// For example, if the user issues a delete request. This will update the metadata on the object
// so we need to replace the spec.
func (j *TrainingJob) Update(newJob *tfv1alpha1.TFJob) {
	j.contextLogger.Info("Updating job to %+v", *newJob)
	j.job = newJob
}

// UID returns the user ID of the requesting user
func (j *TrainingJob) UID() types.UID {
	return j.job.ObjectMeta.UID
}

// ClusterSpec returns the cluster specification for the training job provided
func (j *TrainingJob) ClusterSpec() ClusterSpec {
	clusterSpec := make(ClusterSpec)

	for _, p := range j.Replicas {
		replicaNames := make([]string, 0, *p.Spec.Replicas)

		for i := int32(0); i < *p.Spec.Replicas; i++ {
			replicaNames = append(replicaNames, fmt.Sprintf("%v:%v", p.genName(i), *p.Spec.TFPort))
		}

		clusterSpec[strings.ToLower(string(p.Spec.TFReplicaType))] = replicaNames
	}

	return clusterSpec
}

// deleteResources deletes the replicas it it was created
func (j *TrainingJob) deleteResources() error {
	for _, r := range j.Replicas {
		if err := r.Delete(); err != nil {
			return err
		}
	}

	return nil
}

// GetStatus returns the status of training job provided
func (j *TrainingJob) GetStatus() (tfv1alpha1.State, []*tfv1alpha1.TFReplicaStatus, error) {
	chief := j.job.Spec.TerminationPolicy.Chief
	chiefState := tfv1alpha1.ReplicaStateUnknown

	state := tfv1alpha1.StateUnknown
	replicaStatuses := make([]*tfv1alpha1.TFReplicaStatus, 0)

	// The state for each replica.
	// TODO(jlewi): We will need to modify this code if we want to allow multiples of a given type of replica.
	replicaSetStates := make(map[tfv1alpha1.TFReplicaType]tfv1alpha1.ReplicaState)

	for _, r := range j.Replicas {
		rStatus, err := r.GetStatus()
		if err != nil {
			log.Errorf("GetStatus() for %v returned error; %v", r.Spec.TFReplicaType, err)
		}

		replicaSetStates[r.Spec.TFReplicaType] = rStatus.State

		replicaStatuses = append(replicaStatuses, &rStatus)

		if string(r.Spec.TFReplicaType) == chief.ReplicaName {
			chiefState = r.GetSingleReplicaStatus(int32(chief.ReplicaIndex))
		}
	}

	if chiefState == tfv1alpha1.ReplicaStateRunning {
		state = tfv1alpha1.StateRunning
	} else if chiefState == tfv1alpha1.ReplicaStateFailed {
		state = tfv1alpha1.StateFailed
	} else if chiefState == tfv1alpha1.ReplicaStateSucceeded {
		state = tfv1alpha1.StateSucceeded
	}

	return state, replicaStatuses, nil
}

// isRetryableTerminationState returns true if a container terminated in a state
// that we consider retryable.
func isRetryableTerminationState(s *v1.ContainerStateTerminated) bool {
	// TODO(jlewi): Need to match logic in
	// https://cs.corp.google.com/piper///depot/google3/cloud/ml/beta/job/training_job_state_util.cc?l=88
	if s.Reason == "OOMKilled" {
		// If the user's process causes an OOM and Docker kills the container,
		// the termination reason of ContainerState will be specified to
		// 'OOMKilled'. In this case, we can't assume this to be a retryable error.
		//
		// This check should happen before checking the termination log, since
		// if the container terminated with an OOM, the termination log may not
		// be written.
		return false
	}

	// TODO(jlewi): Should we use the exit code reported in the termination
	// log message and not the ExitCode reported by the container.

	if s.ExitCode >= 0 && s.ExitCode <= 127 {
		// For the exit_code in [0, 127]:
		//   0 means success,
		//   1 - 127 corresponds to permanent user errors.
		// We don't want to retry for both cases.
		// More info about exit status can be found in:
		// https://www.gnu.org/software/bash/manual/html_node/Exit-Status.html
		return false
	}

	// For the remaining cases that exit_code from workers that doesn't
	// fall into [0, 127]. They can be:
	//   137 corresponds to SIGKILL,
	//   143 corresponds to SIGTERM,
	//   other values that have undefined behavior.
	// We treat them as internal errors for now and all the internal errors
	// will be retired.
	return true
}

// masterName returns the name of master replica of provided training job
func (j *TrainingJob) masterName() string {
	return fmt.Sprintf("master-%v-0", j.job.Spec.RuntimeId)
}

// setup the training job.
func (j *TrainingJob) setup(config *tfv1alpha1.ControllerConfig) {
	err := func() error {
		// If the job has already started we shouldn't set it up again.
		if j.status.Phase != tfv1alpha1.TFJobPhaseNone {
			log.Warningf("Job %v has already been setup.", j.name())
			return nil
		}

		// Set defaults.
		scheme.Scheme.Default(j.job)

		err := validation.ValidateTFJobSpec(&j.job.Spec)
		if err != nil {
			return fmt.Errorf("invalid job spec: %v", err)
		}

		if err := helper.ConfigureAcceleratorsForTFJobSpec(&j.job.Spec, config.Accelerators); err != nil {
			return fmt.Errorf("ConfigureAccelerators(...) error; %v", err)
		}

		if j.job.Spec.RuntimeId == "" {
			j.job.Spec.RuntimeId = util.RandString(4)
		}
		return nil
	}()

	if err != nil {
		j.status.Reason = err.Error()
		j.status.Phase = tfv1alpha1.TFJobPhaseFailed
		j.status.State = tfv1alpha1.StateFailed
	} else {
		j.status.Phase = tfv1alpha1.TFJobPhaseCreating
		j.status.State = tfv1alpha1.StateRunning
	}
}

// // setupReplicas creates in memory data structures corresponding to the replicas.
func (j *TrainingJob) setupReplicas() error {
	log.Info("in setupReplicas: ")
	if len(j.Replicas) != len(j.job.Spec.ReplicaSpecs) {
		j.Replicas = make([]*TFReplicaSet, 0, len(j.job.Spec.ReplicaSpecs))
		for _, t := range j.job.Spec.ReplicaSpecs {
			r, err := NewTFReplicaSet(j.KubeCli, j.recorder, *t, j)
			if err != nil {
				return err
			}
			j.Replicas = append(j.Replicas, r)
		}
	}

	return nil
}

// Delete methods deletes the pods and frees the allocated resources
func (j *TrainingJob) Delete() {
	// TODO(jlewi): Delete is what should cause us to delete the Pods.
	// we shouldn't delete the pods when the jobs finish because leaving the pods
	// allows us to get the logs from the pods after the job finishes.
	//
	j.contextLogger.Infof("TFJob %v deleted by the user", j.fullname())
	// TODO(jlewi): This logic is probably insufficient.
	if j.job.Status.Phase != tfv1alpha1.TFJobPhaseCleanUp {
		j.status.Phase = tfv1alpha1.TFJobPhaseCleanUp
	}

	// TODO(jlewi): Does it make sense to explicitly delete the resources? Should
	// we just rely on K8s garbage collection to delete the resources before
	// deleting TFJob?
	if cErr := j.deleteResources(); cErr != nil {
		j.contextLogger.Errorf("trainingJob.deleteResources() error; %v", cErr)
	}

	if j.pdb != nil {
		// if the job has PDB for gang scheduling, delete it
		err := j.KubeCli.PolicyV1beta1().PodDisruptionBudgets(j.job.ObjectMeta.Namespace).Delete(j.pdb.ObjectMeta.Name, &meta_v1.DeleteOptions{})
		if err != nil {
			j.contextLogger.Errorf("Error deleting PDB %v; %v", j.pdb.ObjectMeta.Name, err)
		}
	}
}

// updateCRDStatus updates the job status based on TraingingJob.status.
func (j *TrainingJob) updateCRDStatus() error {
	// If the status hasn't changed then there's no reason to update the CRD.
	if reflect.DeepEqual(j.job.Status, j.status) {
		return nil
	}

	newJob := j.job
	newJob.Status = j.status
	newJob, err := j.tfJobClient.KubeflowV1alpha1().TFJobs(j.job.ObjectMeta.Namespace).Update(newJob)
	if err != nil {
		return err
	}

	j.job = newJob

	return nil
}

/*** Jack Lin ***/

func (j *TrainingJob) EnqueueScheduleSetTime() error {
	j.status.EnqueueScheduleTime = time.Now().Format("2006.01.02-15:04:05")
	if err := j.updateCRDStatus(); err != nil {
		j.contextLogger.Warningf("failed to update CRD status: %v", err)
		return err
	}
	return nil
}

func (j *TrainingJob) StartRunTimeSetTime() error {
	j.status.StartRunTime = time.Now().Format("2006.01.02-15:04:05")
	if err := j.updateCRDStatus(); err != nil {
		j.contextLogger.Warningf("failed to update CRD status: %v", err)
		return err
	}
	return nil
}

func (j *TrainingJob) JobFinishSetTime() error {
	j.status.JobFinishTime = time.Now().Format("2006.01.02-15:04:05")
	if err := j.updateCRDStatus(); err != nil {
		j.contextLogger.Warningf("failed to update CRD status: %v", err)
		return err
	}
	return nil
}

func (j *TrainingJob) SetCurrentRunningReplicas() error {
	var count int

	for _, tempCount := range j.placementPlan {
		count += tempCount
	}

	newJob := j.job
	*newJob.Spec.ReplicaSpecs[0].Replicas = int32(count)
	newJob, err := j.tfJobClient.KubeflowV1alpha1().TFJobs(j.job.ObjectMeta.Namespace).Update(newJob)
	if err != nil {
		return err
	}
	j.job = newJob

	j.currentReplicas = count
	j.status.RunningReplicas = count

	if err := j.updateCRDStatus(); err != nil {
		j.contextLogger.Warningf("failed to update CRD status: %v", err)
		return err
	}

	//refresh j.Replicas
	j.Replicas = make([]*TFReplicaSet, 0, len(j.job.Spec.ReplicaSpecs))
	for _, t := range j.job.Spec.ReplicaSpecs {
		r, err := NewTFReplicaSet(j.KubeCli, j.recorder, *t, j)
		if err != nil {
			return err
		}
		j.Replicas = append(j.Replicas, r)
	}

	if err := j.updateCRDStatus(); err != nil {
		j.contextLogger.Warningf("failed to update CRD status: %v", err)
		return err
	}

	return nil
}

/*
* When a job arrive and before enqueued into scheduleQueueJob
 */
func (j *TrainingJob) ArrivalSetup(config *tfv1alpha1.ControllerConfig) error {
	if j.job.ObjectMeta.DeletionTimestamp != nil {
		j.contextLogger.Info("Deletion timestamp set; skipping reconcile")
		// Job is in the process of being deleted so do nothing.
		// We especially don't want to create new resources as that could block deletion.
		return nil
	}

	if j.job.Status.Phase == tfv1alpha1.TFJobPhaseNone {
		// The job hasn't been setup.
		j.setup(config)

		if err := j.updateCRDStatus(); err != nil {
			j.contextLogger.Warningf("failed to update CRD status: %v", err)
			return err
		}
	}

	// setupreplicas initializes data structures inside TrainingJob representing the replicas.
	// These are go-lang structures which aren't preserved in the APIServer. So we always need to call setupReplicas
	// unlike setup which only needs to be called once during the lifecycle of the job.
	if err := j.setupReplicas(); err != nil {
		j.contextLogger.Errorf("failed to create replicas: %v", err)
		j.status.Reason = fmt.Sprintf("Could not create in memory datastructures; %v", err)
		if uErr := j.updateCRDStatus(); err != nil {
			j.contextLogger.Warningf("Job %v; failed to update status error: %v", j.job.ObjectMeta.Name, uErr)
		}
		return err
	}
	log.Info("in ArrivalSetup finish")
	return nil
}

// when the job is selected and put into running queue, then create the pods of the job for real running
func (j *TrainingJob) CreatePodsAndRunJob(config *tfv1alpha1.ControllerConfig, enableGangScheduling bool, placementPlan map[string]int, PSPlace string) error {

	j.placementPlan = placementPlan
	j.PSPlace = PSPlace
	j.SetCurrentRunningReplicas()

	// sync PDB for gang scheduling
	// TODO(mitake): replace PDB with a newer mechanism if it is replaced
	if enableGangScheduling {
		err := j.syncPdb()
		if err != nil {
			j.contextLogger.Errorf("SyncPdb error: %v", err)
		}
	}

	// Only sync pods and services if we are running.
	if j.status.Phase == tfv1alpha1.TFJobPhaseCreating || j.status.Phase == tfv1alpha1.TFJobPhaseRunning {
		// sync pods
		for _, rc := range j.Replicas {
			err := rc.SyncPods(placementPlan, PSPlace, rc.Spec.TFReplicaType)
			if err != nil {
				j.contextLogger.Errorf("SyncPods error: %v", err)
			}
		}

		// sync services
		for _, rc := range j.Replicas {
			err := rc.SyncServices()
			if err != nil {
				j.contextLogger.Errorf("SyncServices error: %v", err)
			}
		}
	}

	if err := j.updateCRDStatus(); err != nil {
		j.contextLogger.Warningf("Job %v; failed to update status error: %v", j.job.ObjectMeta.Name, err)
		return err
	}
	return nil

}

func (j *TrainingJob) getLogOfPod() (string, error) {

	j.contextLogger.Infof("get Worker0: %v of job: %v", j.Worker0Name, j.job.ObjectMeta.Name)

	client := j.KubeCli.CoreV1().RESTClient()

	pod, err := j.KubeCli.CoreV1().Pods(j.job.ObjectMeta.Namespace).Get(j.Worker0Name, meta_v1.GetOptions{})
	//j.contextLogger.Infof("get Worker0 pods: %v ", pod)

	if err != nil {
		return "", err
	}
	container := pod.Spec.Containers[0].Name
	//j.contextLogger.Infof("get Worker0 pods container: %v ", container)

	logOptions := &api.PodLogOptions{
		Container:  container,
		Follow:     false,
		Previous:   false,
		Timestamps: true,
	}

	req := client.Get().
		Namespace(j.job.ObjectMeta.Namespace).
		Name(j.Worker0Name).
		Resource("pods").
		SubResource("log").
		Param("follow", strconv.FormatBool(logOptions.Follow)).
		Param("container", logOptions.Container).
		Param("previous", strconv.FormatBool(logOptions.Previous)).
		Param("timestamps", strconv.FormatBool(logOptions.Timestamps))

	readCloser, err := req.Stream()
	if err != nil {
		j.contextLogger.Infof("error when readCloser, err := req.Stream() error: %v", err)
		return "", err
	}

	defer readCloser.Close()

	result, err := ioutil.ReadAll(readCloser)
	if err != nil {
		j.contextLogger.Infof("error when result, err := ioutil.ReadAll(readCloser) error: %v", err)
		return "", err
	}

	//j.contextLogger.Infof("get Worker0's log", string(result))

	return string(result), nil
}

// do the real scale:
// get the current training steps
// delete current all pod and service and other resources
// recreate all pod based on new j.placementPlan

func (j *TrainingJob) GetTrainingSteps() (int, error) {

	var trainSteps int = 0

	logs, err := j.getLogOfPod()
	if err != nil {
		j.contextLogger.Infof("error in GetTrainingSteps after getLogOfPod: %v", err)
		return 0, err
	}

	sliceTemp := strings.Split(logs, "\n")
	last := len(sliceTemp)
	if last < 2 {
		trainSteps = 0 // <=
	} else {
		if strings.Contains(sliceTemp[last-2], "images/sec") {
			// last line: "2018-05-09T15:19:36.629802035Z INFO|2018-05-09T15:19:36|/opt/launcher.py|27| 60\timages/sec: 102.4 +/- 0.7 (jitter = 5.3)\t12.258"
			lastLine := sliceTemp[last-2]

			// sliceLastLineTemp: ["2018-05-09T15:19:36.629802035Z INFO|2018-05-09T15:19:36|/opt/launcher.py|27| 60" "images/sec: 102.4 +/- 0.7 (jitter = 5.3)" "12.258"]
			sliceLastLineTemp := strings.Split(lastLine, "\t")

			// getTrainSteps: ["2018-05-09T15:19:36.629802035Z" "INFO|2018-05-09T15:19:36|/opt/launcher.py|27|" "60"]
			getTrainSteps := strings.Split(sliceLastLineTemp[0], " ")

			//lastStep: 60
			lastStep, _ := strconv.Atoi(getTrainSteps[2])

			trainSteps = lastStep // <=

		} else {
			trainSteps = 0 // <=
		}
	}

	j.contextLogger.Infof("Get Workers training steps successfully training steps: %v", trainSteps)

	//Do Scale

	return trainSteps, nil
}

//scale down: -2

//
//job: 5 worker
//{
//	"node1" : 2 workers,
//	"node2" : 3 workers,
//}

func (j *TrainingJob) ScaleDown(scaledownNum int) string {

	node1Num := j.placementPlan["apollo61"]
	node2Num := j.placementPlan["apollo62"]

	var big string
	var small string
	var PSPlace string

	if node1Num > node2Num {
		if node2Num == 0 {
			big = "apollo62"
			small = "apollo61"
			PSPlace = small
		} else {
			big = "apollo61"
			small = "apollo62"
			PSPlace = big
		}
	} else {
		if node1Num == 0 {
			big = "apollo61"
			small = "apollo62"
			PSPlace = small
		} else {
			big = "apollo62"
			small = "apollo61"
			PSPlace = big
		}

	}

	for scaledownNum < 0 || j.placementPlan[small] == 0 {
		j.placementPlan[small] -= 1
		scaledownNum += 1
	}

	if scaledownNum > 0 {
		for scaledownNum < 0 || j.placementPlan[big] == 0 {
			j.placementPlan[big] -= 1
			scaledownNum += 1
		}
	}

	return PSPlace
}

func (j *TrainingJob) ScaleUp(scaleUpNum int) {
	j.placementPlan[j.PSPlace] += scaleUpNum
	return
}

func (j *TrainingJob) DoScale(trainingSteps int) error {

	// delete current worker pods/service and PS pods/service
	if cErr := j.deleteResources(); cErr != nil {
		j.contextLogger.Errorf("trainingJob.deleteResources() error; %v", cErr)
	}

	var originalTrainingSteps int = 0
	var leftTrainingSteps int = 0

	for _, r := range j.Replicas {
		if r.Spec.TFReplicaType == tfv1alpha1.WORKER {
			// --num_batches=200
			argsTempString := r.Spec.Template.Spec.Containers[0].Args[7]
			sliceTemp := strings.Split(argsTempString, "=")
			trainsteps, _ := strconv.Atoi(sliceTemp[1])
			originalTrainingSteps = trainsteps
			break
		}
	}

	// calculate left training steps and update the jobs
	leftTrainingSteps = originalTrainingSteps - trainingSteps
	j.contextLogger.Infof("job: %v original trainsteps: %v, currentTrainSteps: %v, leftTrainingSteps: %v", j.job.ObjectMeta.Name, originalTrainingSteps, trainingSteps, leftTrainingSteps)

	newJob := j.job
	newJob.Spec.ReplicaSpecs[0].Template.Spec.Containers[0].Args[7] = "--num_batches=" + strconv.Itoa(leftTrainingSteps)
	newJob.Spec.ReplicaSpecs[0].Template.Spec.Containers[0].Args[7] = "--num_batches=" + strconv.Itoa(leftTrainingSteps)
	newJob, err := j.tfJobClient.KubeflowV1alpha1().TFJobs(j.job.ObjectMeta.Namespace).Update(newJob)
	if err != nil {
		return err
	}
	j.job = newJob

	// Recreate all the training pods
	if j.status.Phase == tfv1alpha1.TFJobPhaseCreating || j.status.Phase == tfv1alpha1.TFJobPhaseRunning {
		// sync pods
		for _, rc := range j.Replicas {
			err := rc.SyncPods(j.placementPlan, j.PSPlace, rc.Spec.TFReplicaType)
			if err != nil {
				j.contextLogger.Errorf("SyncPods error: %v", err)
			}
		}

		// sync services
		for _, rc := range j.Replicas {
			err := rc.SyncServices()
			if err != nil {
				j.contextLogger.Errorf("SyncServices error: %v", err)
			}
		}
	}

	if err := j.updateCRDStatus(); err != nil {
		j.contextLogger.Warningf("Job %v; failed to update status error: %v", j.job.ObjectMeta.Name, err)
		return err
	}
	return nil

}

// Reconcile tries to get the job into the desired state.
func (j *TrainingJob) Reconcile(scaleNum int, scaledownFlag bool, scaleUpFlag bool) error {
	if scaledownFlag == true {
		j.contextLogger.Infof("job: %v  is going to scale down", j.job.ObjectMeta.Name)
		PSPlace := j.ScaleDown(scaleNum)
		j.PSPlace = PSPlace
		j.contextLogger.Infof("job: %v  after scale down, placementPlan: %v, PSPlace: %v ", j.job.ObjectMeta.Name, j.placementPlan, j.PSPlace)

		j.SetCurrentRunningReplicas()

		trainingSteps, err := j.GetTrainingSteps()
		if err != nil {
			j.contextLogger.Infof("job: %v has error when GetTrainingSteps error: %v ", j.job.ObjectMeta.Name, err)
			return err
		}

		err = j.DoScale(trainingSteps)
		if err != nil {
			j.contextLogger.Infof("job: %v has error when DoScale error: %v ", j.job.ObjectMeta.Name, err)
			return err
		}
		j.contextLogger.Infof("!!!job: %v ScaleDown Successfully!!!", j.job.ObjectMeta.Name)
	}

	if scaleUpFlag == true {
		j.contextLogger.Infof("job: %v  is going to scale UP", j.job.ObjectMeta.Name)
		j.ScaleUp(scaleNum)
		j.contextLogger.Infof("job: %v  after scale UP, placementPlan: %v, PSPlace: %v ", j.job.ObjectMeta.Name, j.placementPlan, j.PSPlace)

		j.SetCurrentRunningReplicas()

		trainingSteps, err := j.GetTrainingSteps()
		if err != nil {
			j.contextLogger.Infof("job: %v has error when GetTrainingSteps error: %v ", j.job.ObjectMeta.Name, err)
			return err
		}
		err = j.DoScale(trainingSteps)
		if err != nil {
			j.contextLogger.Infof("job: %v has error when DoScale error: %v ", j.job.ObjectMeta.Name, err)
			return err
		}
		j.contextLogger.Infof("!!!job: %v ScaleUp Successfully!!!", j.job.ObjectMeta.Name)

	}

	if scaleUpFlag == false && scaledownFlag == false {
		j.SetCurrentRunningReplicas()
	}

	// Call GetStatus in each reconcile loop
	state, replicaStatuses, err := j.GetStatus()

	j.status.ReplicaStatuses = replicaStatuses
	if err != nil {
		j.contextLogger.Errorf("GetStatus() for job %v returned error: %v", j.job.ObjectMeta.Name, err)
		return err
	}

	// TODO(jlewi): We should update the Phase if we detect the job is done.
	if state == tfv1alpha1.StateFailed {
		j.contextLogger.Errorf("Master failed Job: %v.", j.job.ObjectMeta.Name)
		j.status.Phase = tfv1alpha1.TFJobPhaseDone
		j.status.State = tfv1alpha1.StateFailed
	} else if state == tfv1alpha1.StateSucceeded {
		j.contextLogger.Infof("Master succeeded Job: %v.", j.job.ObjectMeta.Name)
		j.status.Phase = tfv1alpha1.TFJobPhaseDone
		j.status.State = tfv1alpha1.StateSucceeded
	} else if state == tfv1alpha1.StateRunning {
		j.contextLogger.Infof("Master running Job: %v.", j.job.ObjectMeta.Name)
		j.status.Phase = tfv1alpha1.TFJobPhaseRunning
		j.status.State = tfv1alpha1.StateRunning
	} else {
		j.contextLogger.Infof("Job %v status=%v", j.job.ObjectMeta.Name, util.Pformat(j.status))
	}

	// If the phase changed we should update the CRD.
	if err := j.updateCRDStatus(); err != nil {
		j.contextLogger.Warningf("Job %v, failed to update CRD status error: %v", j.job.ObjectMeta.Name, err)
		return err
	}

	if j.job.Status.Phase == tfv1alpha1.TFJobPhaseCleanUp {
		if cErr := j.deleteResources(); cErr != nil {
			j.contextLogger.Errorf("Job %v trainingJob.Delete() error; %v", j.job.ObjectMeta.Name, cErr)
		}
		// j.status.SetPhase(spec.TFJobPhaseDone)
		// Return from run because we want to stop reconciling the object.
		return nil
	}

	// updateCRDStatus will update the status of the CRD with c.Status if c.Status
	// doesn't match c.Cluster.status. So you can change c.Status in order to propagate
	// changes to the CRD status.
	if err := j.updateCRDStatus(); err != nil {
		j.contextLogger.Warningf("Job %v; failed to update CRD status error: %v", j.job.ObjectMeta.Name, err)
		return err
	}

	return nil
}

// name returns the name of the given training job.
func (j *TrainingJob) name() string {
	return j.job.ObjectMeta.GetName()
}

// fullname returns the namespace and name for the job.
func (j *TrainingJob) fullname() string {
	return j.job.ObjectMeta.GetNamespace() + ":" + j.job.ObjectMeta.GetName()
}

// SchedulerName returns the scheduler name for the job.
func (j *TrainingJob) SchedulerName() string {
	return j.job.Spec.SchedulerName
}

// SyncPdb will create a PDB for gang scheduling by kube-arbitrator.
func (j *TrainingJob) syncPdb() error {
	nrReplicas := int32(0)
	for _, r := range j.Replicas {
		nrReplicas += *r.Spec.Replicas
	}

	if nrReplicas == 1 {
		// gang scheduling isn't required by a non distributed training process
		return nil
	}

	minAvailable := intstr.FromInt(int(nrReplicas))
	pdb := &v1beta1.PodDisruptionBudget{
		ObjectMeta: meta_v1.ObjectMeta{
			//GenerateName: "tf-job-pdb-",
			Name: "tf-job-pdb-" + j.job.ObjectMeta.Name,
			OwnerReferences: []meta_v1.OwnerReference{
				helper.AsOwner(j.job),
			},
		},
		Spec: v1beta1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvailable,
			Selector: &meta_v1.LabelSelector{
				MatchLabels: map[string]string{
					"runtime_id":  j.job.Spec.RuntimeId,
					"tf_job_name": j.job.ObjectMeta.Name,
				},
			},
		},
	}

	createdPdb, err := j.KubeCli.PolicyV1beta1().PodDisruptionBudgets(j.job.ObjectMeta.Namespace).Create(pdb)
	if err != nil {
		if k8s_errors.IsAlreadyExists(err) {
			j.contextLogger.Infof("PDB: %v already exists.", "tf-job-pdb-"+j.job.ObjectMeta.Name)
			return nil
		}

		j.recorder.Eventf(j.job, v1.EventTypeWarning, FailedCreateReason, "Error creating: %v", err)
		return err
	}

	j.pdb = createdPdb

	j.recorder.Eventf(j.job, v1.EventTypeNormal, SuccessfulCreateReason, "Created PDB: %v", createdPdb.Name)
	return nil
}
