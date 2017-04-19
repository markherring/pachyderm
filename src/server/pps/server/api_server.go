package server

import (
	"bufio"
	"bytes"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	client "github.com/pachyderm/pachyderm/src/client"
	"github.com/pachyderm/pachyderm/src/client/pfs"
	"github.com/pachyderm/pachyderm/src/client/pkg/uuid"
	"github.com/pachyderm/pachyderm/src/client/pps"
	"github.com/pachyderm/pachyderm/src/server/pkg/backoff"
	col "github.com/pachyderm/pachyderm/src/server/pkg/collection"
	"github.com/pachyderm/pachyderm/src/server/pkg/hashtree"
	"github.com/pachyderm/pachyderm/src/server/pkg/metrics"
	"github.com/pachyderm/pachyderm/src/server/pkg/obj"
	pfs_sync "github.com/pachyderm/pachyderm/src/server/pkg/sync"
	"github.com/pachyderm/pachyderm/src/server/pkg/watch"
	workerpkg "github.com/pachyderm/pachyderm/src/server/pkg/worker"
	ppsserver "github.com/pachyderm/pachyderm/src/server/pps"

	etcd "github.com/coreos/etcd/clientv3"
	"github.com/gogo/protobuf/jsonpb"
	"github.com/gogo/protobuf/proto"
	"github.com/gogo/protobuf/types"
	"go.pedge.io/lion/proto"
	"go.pedge.io/proto/rpclog"
	"golang.org/x/net/context"
	"google.golang.org/grpc"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/errors"
	"k8s.io/kubernetes/pkg/api/unversioned"
	kube "k8s.io/kubernetes/pkg/client/unversioned"
	kube_labels "k8s.io/kubernetes/pkg/labels"
)

const (
	// MaxPodsPerChunk is the maximum number of pods we can schedule for each
	// chunk in case of failures.
	MaxPodsPerChunk = 3
	// DefaultUserImage is the image used for jobs when the user does not specify
	// an image.
	DefaultUserImage = "ubuntu:16.04"
	// MaximumRetriesPerDatum is the maximum number of times each datum
	// can failed to be processed before we declare that the job has failed.
	MaximumRetriesPerDatum = 3
)

var (
	trueVal = true
	suite   = "pachyderm"
)

func newErrJobNotFound(job string) error {
	return fmt.Errorf("job %v not found", job)
}

func newErrPipelineNotFound(pipeline string) error {
	return fmt.Errorf("pipeline %v not found", pipeline)
}

func newErrPipelineExists(pipeline string) error {
	return fmt.Errorf("pipeline %v already exists", pipeline)
}

type errEmptyInput struct {
	error
}

func newErrEmptyInput(commitID string) *errEmptyInput {
	return &errEmptyInput{
		error: fmt.Errorf("job was not started due to empty input at commit %v", commitID),
	}
}

func newErrParentInputsMismatch(parent string) error {
	return fmt.Errorf("job does not have the same set of inputs as its parent %v", parent)
}

type ctxAndCancel struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// GetExpectedNumWorkers computes the expected number of workers that pachyderm will start given
// the ParallelismSpec 'spec'.
//
// This is only exported for testing
func GetExpectedNumWorkers(kubeClient *kube.Client, spec *pps.ParallelismSpec) (uint64, error) {
	coefficient := 0.0 // Used if [spec.Strategy == PROPORTIONAL] or [spec.Constant == 0]
	if spec == nil {
		// Unset ParallelismSpec is handled here. Currently we start one worker per
		// node
		coefficient = 1.0
	} else if spec.Strategy == pps.ParallelismSpec_CONSTANT {
		if spec.Constant > 0 {
			fmt.Println("Returning constant: ", spec.Constant)
			return spec.Constant, nil
		}
		// Zero-initialized ParallelismSpec is handled here. Currently we start one
		// worker per node
		coefficient = 1
	} else if spec.Strategy == pps.ParallelismSpec_COEFFICIENT {
		coefficient = spec.Coefficient
	} else {
		return 0, fmt.Errorf("Unable to interpret ParallelismSpec strategy %s", spec.Strategy)
	}
	if coefficient == 0.0 {
		return 0, fmt.Errorf("Ended up with coefficient == 0 (no workers) after interpreting ParallelismSpec %s", spec.Strategy)
	}

	// Start ('coefficient' * 'nodes') workers. Determine number of workers
	nodeList, err := kubeClient.Nodes().List(api.ListOptions{})
	if err != nil {
		return 0, fmt.Errorf("unable to retrieve node list from k8s to determine parallelism: %v", err)
	}
	if len(nodeList.Items) == 0 {
		return 0, fmt.Errorf("pachyderm.pps.jobserver: no k8s nodes found")
	}
	result := math.Floor(coefficient * float64(len(nodeList.Items)))
	return uint64(math.Max(result, 1)), nil
}

type apiServer struct {
	protorpclog.Logger
	etcdPrefix          string
	hasher              *ppsserver.Hasher
	address             string
	etcdClient          *etcd.Client
	pachConn            *grpc.ClientConn
	pachConnOnce        sync.Once
	kubeClient          *kube.Client
	shardLock           sync.RWMutex
	shardCtxs           map[uint64]*ctxAndCancel
	pipelineCancelsLock sync.Mutex
	pipelineCancels     map[string]context.CancelFunc

	// lock for 'jobCancels'
	jobCancelsLock sync.Mutex
	jobCancels     map[string]context.CancelFunc
	version        int64
	// versionLock protects the version field.
	// versionLock must be held BEFORE reading from version and UNTIL all
	// requests using version have returned
	versionLock           sync.RWMutex
	namespace             string
	workerImage           string
	workerImagePullPolicy string
	reporter              *metrics.Reporter
	// collections
	pipelines col.Collection
	jobs      col.Collection
}

func (a *apiServer) validateInput(ctx context.Context, input *pps.Input, job bool) error {
	names := make(map[string]bool)
	set := false
	if input.Atom != nil {
		set = true
		switch {
		case len(input.Atom.Name) == 0:
			return fmt.Errorf("input must specify a name")
		case input.Atom.Name == "out":
			return fmt.Errorf("input cannot be named \"out\", as pachyderm " +
				"already creates /pfs/out to collect job output")
		case input.Atom.Commit == nil:
			return fmt.Errorf("input must specify a commit")
		case len(input.Atom.Commit.ID) == 0:
			if job {
				return fmt.Errorf("input must specify a commit ID")
			} else {
				return fmt.Errorf("input must specify a branch")
			}
		case len(input.Atom.Glob) == 0:
			return fmt.Errorf("input must specify a glob")
		}
		if _, ok := names[input.Atom.Name]; ok {
			return fmt.Errorf("conflicting input names: %s", input.Atom.Name)
		}
		names[input.Atom.Name] = true
		pfsClient, err := a.getPFSClient()
		if err != nil {
			return err
		}
		if job {
			// for jobs we check that the input commit exists
			_, err = pfsClient.InspectCommit(ctx, &pfs.InspectCommitRequest{
				Commit: input.Atom.Commit,
			})
			if err != nil {
				return err
			}
		} else {
			// for pipelines we only check that the repo exists
			_, err = pfsClient.InspectRepo(ctx, &pfs.InspectRepoRequest{
				Repo: input.Atom.Commit.Repo,
			})
			if err != nil {
				return err
			}
		}
	}
	if input.Cross != nil {
		if set {
			return fmt.Errorf("multiple input types set")
		}
		set = true
		for _, input := range input.Cross.Input {
			if err := a.validateInput(ctx, input, job); err != nil {
				return err
			}
		}
	}
	if input.Union != nil {
		if set {
			return fmt.Errorf("multiple input types set")
		}
		set = true
		for _, input := range input.Cross.Input {
			if err := a.validateInput(ctx, input, job); err != nil {
				return err
			}
		}
	}
	return nil
}

// visit each input recursively in ascending order (root last)
func visit(input *pps.Input, f func(*pps.Input)) {
	switch {
	case input.Cross != nil:
		for _, input := range input.Cross.Input {
			visit(input, f)
		}
	case input.Union != nil:
		for _, input := range input.Cross.Input {
			visit(input, f)
		}
	}
	f(input)
}

func name(input *pps.Input) string {
	switch {
	case input.Atom != nil:
		return input.Atom.Name
	case input.Cross != nil:
		if len(input.Cross.Input) == 0 {
			return ""
		}
		return name(input.Cross.Input[0])
	case input.Union != nil:
		if len(input.Union.Input) == 0 {
			return ""
		}
		return name(input.Union.Input[0])
	}
	return ""
}

func sortInput(input *pps.Input) {
	visit(input, func(input *pps.Input) {
		sortInputs := func(inputs []*pps.Input) {
			sort.SliceStable(inputs, func(i, j int) bool { return name(inputs[i]) < name(inputs[j]) })
		}
		switch {
		case input.Cross != nil:
			sortInputs(input.Cross.Input)
		case input.Union != nil:
			sortInputs(input.Union.Input)
		}
	})
}

func inputCommits(input *pps.Input) []*pfs.Commit {
	var result []*pfs.Commit
	visit(input, func(input *pps.Input) {
		if input.Atom != nil {
			result = append(result, input.Atom.Commit)
		}
	})
	return result
}

func (a *apiServer) validateJob(ctx context.Context, jobInfo *pps.JobInfo) error {
	return a.validateInput(ctx, jobInfo.Input, true)
}

func translateJobInputs(inputs []*pps.JobInput) *pps.Input {
	result := &pps.Input{Cross: &pps.CrossInput{}}
	for _, input := range inputs {
		result.Cross.Input = append(result.Cross.Input,
			&pps.Input{
				Atom: &pps.AtomInput{
					Name:   input.Name,
					Commit: input.Commit,
					Glob:   input.Glob,
					Lazy:   input.Lazy,
				},
			})
	}
	return result
}

func (a *apiServer) CreateJob(ctx context.Context, request *pps.CreateJobRequest) (response *pps.Job, retErr error) {
	func() { a.Log(request, nil, nil, 0) }()
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())
	metricsFn := metrics.ReportUserAction(ctx, a.reporter, "CreateJob")
	defer func(start time.Time) { metricsFn(start, retErr) }(time.Now())

	// First translate Inputs field to Input field.
	if len(request.Inputs) > 0 {
		if request.Input != nil {
			return nil, fmt.Errorf("cannot set both Inputs and Input field")
		}
		request.Input = translateJobInputs(request.Inputs)
	}

	job := &pps.Job{uuid.NewWithoutUnderscores()}
	sortInput(request.Input)
	_, err := col.NewSTM(ctx, a.etcdClient, func(stm col.STM) error {
		jobInfo := &pps.JobInfo{
			Job:             job,
			Transform:       request.Transform,
			Pipeline:        request.Pipeline,
			ParallelismSpec: request.ParallelismSpec,
			Input:           request.Input,
			OutputRepo:      request.OutputRepo,
			OutputBranch:    request.OutputBranch,
			Started:         now(),
			Finished:        nil,
			OutputCommit:    nil,
			Service:         request.Service,
			ParentJob:       request.ParentJob,
		}
		if request.Pipeline != nil {
			pipelineInfo := new(pps.PipelineInfo)
			if err := a.pipelines.ReadWrite(stm).Get(request.Pipeline.Name, pipelineInfo); err != nil {
				return err
			}
			jobInfo.PipelineVersion = pipelineInfo.Version
			jobInfo.PipelineID = pipelineInfo.ID
			jobInfo.Transform = pipelineInfo.Transform
			jobInfo.ParallelismSpec = pipelineInfo.ParallelismSpec
			jobInfo.OutputRepo = &pfs.Repo{pipelineInfo.Pipeline.Name}
			jobInfo.OutputBranch = pipelineInfo.OutputBranch
			jobInfo.Egress = pipelineInfo.Egress
		} else {
			if jobInfo.OutputRepo == nil {
				jobInfo.OutputRepo = &pfs.Repo{job.ID}
			}
		}
		if err := a.validateJob(ctx, jobInfo); err != nil {
			return err
		}
		return a.updateJobState(stm, jobInfo, pps.JobState_JOB_STARTING)
	})
	if err != nil {
		return nil, err
	}
	return job, nil
}

func (a *apiServer) InspectJob(ctx context.Context, request *pps.InspectJobRequest) (response *pps.JobInfo, retErr error) {
	func() { a.Log(request, nil, nil, 0) }()
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())
	metricsFn := metrics.ReportUserAction(ctx, a.reporter, "InspectJob")
	defer func(start time.Time) { metricsFn(start, retErr) }(time.Now())

	jobs := a.jobs.ReadOnly(ctx)

	if request.BlockState {
		watcher, err := jobs.WatchOne(request.Job.ID)
		if err != nil {
			return nil, err
		}
		defer watcher.Close()

		for {
			ev, ok := <-watcher.Watch()
			if !ok {
				return nil, fmt.Errorf("the stream for job updates closed unexpectedly")
			}
			switch ev.Type {
			case watch.EventError:
				return nil, ev.Err
			case watch.EventDelete:
				return nil, fmt.Errorf("job %s was deleted", request.Job.ID)
			case watch.EventPut:
				var jobID string
				var jobInfo pps.JobInfo
				if err := ev.Unmarshal(&jobID, &jobInfo); err != nil {
					return nil, err
				}
				if jobStateToStopped(jobInfo.State) {
					return &jobInfo, nil
				}
			}
		}
	}

	jobInfo := new(pps.JobInfo)
	if err := jobs.Get(request.Job.ID, jobInfo); err != nil {
		return nil, err
	}
	if jobInfo.Input == nil {
		jobInfo.Input = translateJobInputs(jobInfo.Inputs)
	}
	// If the job is running we fill in WorkerStatus field, otherwise we just
	// return the jobInfo.
	if jobInfo.State != pps.JobState_JOB_RUNNING {
		return jobInfo, nil
	}
	var workerPoolID string
	if jobInfo.Pipeline != nil {
		workerPoolID = PipelineRcName(jobInfo.Pipeline.Name, jobInfo.PipelineVersion)
	} else {
		workerPoolID = JobRcName(jobInfo.Job.ID)
	}
	workerStatus, err := status(ctx, workerPoolID, a.etcdClient, a.etcdPrefix)
	if err != nil {
		protolion.Errorf("failed to get worker status with err: %s", err.Error())
	} else {
		// It's possible that the workers might be working on datums for other
		// jobs, we omit those since they're not part of the status for this
		// job.
		for _, status := range workerStatus {
			if status.JobID == jobInfo.Job.ID {
				jobInfo.WorkerStatus = append(jobInfo.WorkerStatus, status)
			}
		}
	}
	return jobInfo, nil
}

func (a *apiServer) ListJob(ctx context.Context, request *pps.ListJobRequest) (response *pps.JobInfos, retErr error) {
	func() { a.Log(request, nil, nil, 0) }()
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())
	metricsFn := metrics.ReportUserAction(ctx, a.reporter, "ListJob")
	defer func(start time.Time) { metricsFn(start, retErr) }(time.Now())

	jobs := a.jobs.ReadOnly(ctx)
	var iter col.Iterator
	var err error
	if request.Pipeline != nil {
		iter, err = jobs.GetByIndex(jobsPipelineIndex, request.Pipeline)
		if err != nil {
			return nil, err
		}
	} else {
		iter, err = jobs.List()
	}

	var jobInfos []*pps.JobInfo
	for {
		var jobID string
		var jobInfo pps.JobInfo
		ok, err := iter.Next(&jobID, &jobInfo)
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		if jobInfo.Input == nil {
			jobInfo.Input = translateJobInputs(jobInfo.Inputs)
		}
		jobInfos = append(jobInfos, &jobInfo)
	}

	return &pps.JobInfos{jobInfos}, nil
}

func (a *apiServer) DeleteJob(ctx context.Context, request *pps.DeleteJobRequest) (response *types.Empty, retErr error) {
	func() { a.Log(request, nil, nil, 0) }()
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())
	metricsFn := metrics.ReportUserAction(ctx, a.reporter, "DeleteJob")
	defer func(start time.Time) { metricsFn(start, retErr) }(time.Now())

	_, err := col.NewSTM(ctx, a.etcdClient, func(stm col.STM) error {
		return a.jobs.ReadWrite(stm).Delete(request.Job.ID)
	})
	if err != nil {
		return nil, err
	}
	return &types.Empty{}, nil
}

func (a *apiServer) StopJob(ctx context.Context, request *pps.StopJobRequest) (response *types.Empty, retErr error) {
	func() { a.Log(request, nil, nil, 0) }()
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())
	metricsFn := metrics.ReportUserAction(ctx, a.reporter, "StopJob")
	defer func(start time.Time) { metricsFn(start, retErr) }(time.Now())

	_, err := col.NewSTM(ctx, a.etcdClient, func(stm col.STM) error {
		jobs := a.jobs.ReadWrite(stm)
		jobInfo := new(pps.JobInfo)
		if err := jobs.Get(request.Job.ID, jobInfo); err != nil {
			return err
		}
		return a.updateJobState(stm, jobInfo, pps.JobState_JOB_STOPPED)
	})
	if err != nil {
		return nil, err
	}
	return &types.Empty{}, nil
}

func (a *apiServer) RestartDatum(ctx context.Context, request *pps.RestartDatumRequest) (response *types.Empty, retErr error) {
	func() { a.Log(request, nil, nil, 0) }()
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())
	metricsFn := metrics.ReportUserAction(ctx, a.reporter, "RestartDatum")
	defer func(start time.Time) { metricsFn(start, retErr) }(time.Now())

	jobInfo, err := a.InspectJob(ctx, &pps.InspectJobRequest{
		Job: request.Job,
	})
	if err != nil {
		return nil, err
	}
	var workerPoolID string
	if jobInfo.Pipeline != nil {
		workerPoolID = PipelineRcName(jobInfo.Pipeline.Name, jobInfo.PipelineVersion)
	} else {
		workerPoolID = JobRcName(jobInfo.Job.ID)
	}
	if err := cancel(ctx, workerPoolID, a.etcdClient, a.etcdPrefix, request.Job.ID, request.DataFilters); err != nil {
		return nil, err
	}
	return &types.Empty{}, nil
}

func (a *apiServer) lookupRcNameForPipeline(ctx context.Context, pipeline *pps.Pipeline) (string, error) {
	var pipelineInfo pps.PipelineInfo
	err := a.pipelines.ReadOnly(ctx).Get(pipeline.Name, &pipelineInfo)
	if err != nil {
		return "", fmt.Errorf("could not get pipeline information for %s: %s", pipeline.Name, err.Error())
	}
	return PipelineRcName(pipeline.Name, pipelineInfo.Version), nil
}

func (a *apiServer) GetLogs(request *pps.GetLogsRequest, apiGetLogsServer pps.API_GetLogsServer) (retErr error) {
	func() { a.Log(request, nil, nil, 0) }()
	defer func(start time.Time) { a.Log(request, nil, retErr, time.Since(start)) }(time.Now())
	// No deadline in request, but we create one here, since we do expect the call
	// to finish reasonably quickly
	ctx, _ := context.WithTimeout(context.Background(), 60*time.Second)

	// Validate request
	if request.Pipeline == nil && request.Job == nil {
		return fmt.Errorf("must set either pipeline or job filter in call to GetLogs")
	}

	// Get list of pods containing logs we're interested in (based on pipeline and
	// job filters)
	var rcName string
	if request.Pipeline != nil {
		// If the user provides a pipeline, get logs from the pipeline RC directly
		var err error
		rcName, err = a.lookupRcNameForPipeline(ctx, request.Pipeline)
		if err != nil {
			return err
		}
	} else if request.Job != nil {
		// If they only provided a job, get job info to see if it's an orphan job
		var jobInfo pps.JobInfo
		err := a.jobs.ReadOnly(ctx).Get(request.Job.ID, &jobInfo)
		if err != nil {
			return fmt.Errorf("could not get job information for %s: %s", request.Job.ID, err.Error())
		}

		// Get logs from either pipeline RC, or job RC if it's an orphan job
		if jobInfo.Pipeline != nil {
			var err error
			rcName, err = a.lookupRcNameForPipeline(ctx, jobInfo.Pipeline)
			if err != nil {
				return err
			}
		} else {
			rcName = JobRcName(request.Job.ID)
		}
	} else {
		return fmt.Errorf("must specify either pipeline or job")
	}
	pods, err := a.rcPods(rcName)
	if err != nil {
		return fmt.Errorf("could not get pods in rc %s containing logs", rcName)
	}
	if len(pods) == 0 {
		return fmt.Errorf("no pods belonging to the rc \"%s\" were found", rcName)
	}

	// Spawn one goroutine per pod. Each goro writes its pod's logs to a channel
	// and channels are read into the output server in a stable order.
	// (sort the pods to make sure that the order of log lines is stable)
	sort.Sort(podSlice(pods))
	logChs := make([]chan *pps.LogMessage, len(pods))
	errCh := make(chan error)
	done := make(chan struct{})
	defer close(done)
	for i := 0; i < len(pods); i++ {
		logChs[i] = make(chan *pps.LogMessage)
	}
	for i, pod := range pods {
		i := i
		pod := pod
		go func() {
			defer close(logChs[i]) // Main thread reads from here, so must close
			// Get full set of logs from pod i
			result := a.kubeClient.Pods(a.namespace).GetLogs(
				pod.ObjectMeta.Name, &api.PodLogOptions{}).Do()
			fullLogs, err := result.Raw()
			if err != nil {
				if apiStatus, ok := err.(errors.APIStatus); ok &&
					strings.Contains(apiStatus.Status().Message, "PodInitializing") {
					return // No logs to collect from this node, just skip it
				}
				select {
				case errCh <- err:
				case <-done:
				}
				return
			}

			// Parse pods' log lines, and filter out irrelevant ones
			scanner := bufio.NewScanner(bytes.NewReader(fullLogs))
			for scanner.Scan() {
				logBytes := scanner.Bytes()
				msg := new(pps.LogMessage)
				if err := jsonpb.Unmarshal(bytes.NewReader(logBytes), msg); err != nil {
					select {
					case errCh <- err:
					case <-done:
					}
					return
				}

				// Filter out log lines that don't match on pipeline or job
				if request.Pipeline != nil && request.Pipeline.Name != msg.PipelineName {
					continue
				}
				if request.Job != nil && request.Job.ID != msg.JobID {
					continue
				}

				if !workerpkg.MatchDatum(request.DataFilters, msg.Data) {
					continue
				}

				// Log message passes all filters -- return it
				select {
				case logChs[i] <- msg:
				case <-done:
					return
				}
			}
		}()
	}
nextLogCh:
	for _, logCh := range logChs {
		for {
			select {
			case msg, ok := <-logCh:
				if !ok {
					continue nextLogCh
				}
				if err := apiGetLogsServer.Send(msg); err != nil {
					return err
				}
			case err := <-errCh:
				return err
			}
		}
	}
	return nil
}

func (a *apiServer) validatePipeline(ctx context.Context, pipelineInfo *pps.PipelineInfo) error {
	return a.validateInput(ctx, pipelineInfo.Input, false)
	if pipelineInfo.OutputBranch == "" {
		return fmt.Errorf("pipeline needs to specify an output branch")
	}
	return nil
}

func translatePipelineInputs(inputs []*pps.PipelineInput) *pps.Input {
	result := &pps.Input{Cross: &pps.CrossInput{}}
	for _, input := range inputs {
		var fromCommitID string
		if input.From != nil {
			fromCommitID = input.From.ID
		}
		result.Cross.Input = append(result.Cross.Input,
			&pps.Input{
				Atom: &pps.AtomInput{
					Name:         input.Name,
					Commit:       client.NewCommit(input.Repo.Name, input.Branch),
					Glob:         input.Glob,
					Lazy:         input.Lazy,
					FromCommitID: fromCommitID,
				},
			})
	}
	return result
}

func (a *apiServer) CreatePipeline(ctx context.Context, request *pps.CreatePipelineRequest) (response *types.Empty, retErr error) {
	func() { a.Log(request, nil, nil, 0) }()
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())
	metricsFn := metrics.ReportUserAction(ctx, a.reporter, "CreatePipeline")
	defer func(start time.Time) { metricsFn(start, retErr) }(time.Now())
	// First translate Inputs field to Input field.
	if len(request.Inputs) > 0 {
		if request.Input != nil {
			return nil, fmt.Errorf("cannot set both Inputs and Input field")
		}
		request.Input = translatePipelineInputs(request.Inputs)
	}

	pipelineInfo := &pps.PipelineInfo{
		ID:                 uuid.NewWithoutDashes(),
		Pipeline:           request.Pipeline,
		Version:            1,
		Transform:          request.Transform,
		ParallelismSpec:    request.ParallelismSpec,
		Input:              request.Input,
		OutputBranch:       request.OutputBranch,
		Egress:             request.Egress,
		CreatedAt:          now(),
		ScaleDownThreshold: request.ScaleDownThreshold,
	}
	setPipelineDefaults(pipelineInfo)
	if err := a.validatePipeline(ctx, pipelineInfo); err != nil {
		return nil, err
	}

	pfsClient, err := a.getPFSClient()
	if err != nil {
		return nil, err
	}

	pipelineName := pipelineInfo.Pipeline.Name

	sortInput(pipelineInfo.Input)
	if request.Update {
		if _, err := a.StopPipeline(ctx, &pps.StopPipelineRequest{request.Pipeline}); err != nil {
			return nil, err
		}
		var oldPipelineInfo pps.PipelineInfo
		_, err := col.NewSTM(ctx, a.etcdClient, func(stm col.STM) error {
			pipelines := a.pipelines.ReadWrite(stm)
			if err := pipelines.Get(pipelineName, &oldPipelineInfo); err != nil {
				return err
			}
			pipelineInfo.Version = oldPipelineInfo.Version + 1
			pipelines.Put(pipelineName, pipelineInfo)
			return nil
		})
		if err != nil {
			return nil, err
		}

		// Rename the original output branch to `outputBranch-vN`, where N
		// is the previous version number of the pipeline.
		// We ignore NotFound errors because this pipeline might not have
		// even output anything yet, in which case the output branch
		// may not actually exist.
		if _, err := pfsClient.SetBranch(ctx, &pfs.SetBranchRequest{
			Commit: &pfs.Commit{
				Repo: &pfs.Repo{pipelineName},
				ID:   oldPipelineInfo.OutputBranch,
			},
			Branch: fmt.Sprintf("%s-v%d", oldPipelineInfo.OutputBranch, oldPipelineInfo.Version),
		}); err != nil && !isNotFoundErr(err) {
			return nil, err
		}

		if _, err := pfsClient.DeleteBranch(ctx, &pfs.DeleteBranchRequest{
			Repo:   &pfs.Repo{pipelineName},
			Branch: oldPipelineInfo.OutputBranch,
		}); err != nil && !isNotFoundErr(err) {
			return nil, err
		}

		if _, err := a.StartPipeline(ctx, &pps.StartPipelineRequest{request.Pipeline}); err != nil {
			return nil, err
		}
	} else {
		_, err := col.NewSTM(ctx, a.etcdClient, func(stm col.STM) error {
			pipelines := a.pipelines.ReadWrite(stm)
			err := pipelines.Create(pipelineName, pipelineInfo)
			if isAlreadyExistsErr(err) {
				return newErrPipelineExists(pipelineName)
			}
			return err
		})
		if err != nil {
			return nil, err
		}
	}

	// Create output repo
	// The pipeline manager also creates the output repo, but we want to
	// also create the repo here to make sure that the output repo is
	// guaranteed to be there after CreatePipeline returns.  This is
	// because it's a very common pattern to create many pipelines in a
	// row, some of which depend on the existence of the output repos
	// of upstream pipelines.
	var provenance []*pfs.Repo
	for _, commit := range inputCommits(pipelineInfo.Input) {
		provenance = append(provenance, commit.Repo)
	}

	if _, err := pfsClient.CreateRepo(ctx, &pfs.CreateRepoRequest{
		Repo:       &pfs.Repo{pipelineInfo.Pipeline.Name},
		Provenance: provenance,
	}); err != nil && !isAlreadyExistsErr(err) {
		return nil, err
	}

	return &types.Empty{}, err
}

// setPipelineDefaults sets the default values for a pipeline info
func setPipelineDefaults(pipelineInfo *pps.PipelineInfo) {
	visit(pipelineInfo.Input, func(input *pps.Input) {
		if input.Atom != nil {
			if input.Atom.Commit.ID == "" {
				input.Atom.Commit.ID = "master"
			}
			if input.Atom.Name == "" {
				input.Atom.Name = input.Atom.Commit.Repo.Name
			}
		}
	})
	if pipelineInfo.OutputBranch == "" {
		// Output branches default to master
		pipelineInfo.OutputBranch = "master"
	}
}

func (a *apiServer) InspectPipeline(ctx context.Context, request *pps.InspectPipelineRequest) (response *pps.PipelineInfo, retErr error) {
	func() { a.Log(request, nil, nil, 0) }()
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())
	metricsFn := metrics.ReportUserAction(ctx, a.reporter, "InspectPipeline")
	defer func(start time.Time) { metricsFn(start, retErr) }(time.Now())

	pipelineInfo := new(pps.PipelineInfo)
	if err := a.pipelines.ReadOnly(ctx).Get(request.Pipeline.Name, pipelineInfo); err != nil {
		return nil, err
	}
	if pipelineInfo.Input == nil {
		pipelineInfo.Input = translatePipelineInputs(pipelineInfo.Inputs)
	}
	return pipelineInfo, nil
}

func (a *apiServer) ListPipeline(ctx context.Context, request *pps.ListPipelineRequest) (response *pps.PipelineInfos, retErr error) {
	func() { a.Log(request, nil, nil, 0) }()
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())
	metricsFn := metrics.ReportUserAction(ctx, a.reporter, "ListPipeline")
	defer func(start time.Time) { metricsFn(start, retErr) }(time.Now())

	pipelineIter, err := a.pipelines.ReadOnly(ctx).List()
	if err != nil {
		return nil, err
	}

	pipelineInfos := new(pps.PipelineInfos)

	for {
		var pipelineName string
		pipelineInfo := new(pps.PipelineInfo)
		ok, err := pipelineIter.Next(&pipelineName, pipelineInfo)
		if err != nil {
			return nil, err
		}
		if ok {
			if pipelineInfo.Input == nil {
				pipelineInfo.Input = translatePipelineInputs(pipelineInfo.Inputs)
			}
			pipelineInfos.PipelineInfo = append(pipelineInfos.PipelineInfo, pipelineInfo)
		} else {
			break
		}
	}
	return pipelineInfos, nil
}

func (a *apiServer) DeletePipeline(ctx context.Context, request *pps.DeletePipelineRequest) (response *types.Empty, retErr error) {
	func() { a.Log(request, nil, nil, 0) }()
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())
	metricsFn := metrics.ReportUserAction(ctx, a.reporter, "DeletePipeline")
	defer func(start time.Time) { metricsFn(start, retErr) }(time.Now())

	iter, err := a.jobs.ReadOnly(ctx).GetByIndex(jobsPipelineIndex, request.Pipeline)
	if err != nil {
		return nil, err
	}

	for {
		var jobID string
		var jobInfo pps.JobInfo
		ok, err := iter.Next(&jobID, &jobInfo)
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		if request.DeleteJobs {
			if _, err := a.DeleteJob(ctx, &pps.DeleteJobRequest{&pps.Job{jobID}}); err != nil {
				return nil, err
			}
		} else {
			if !jobStateToStopped(jobInfo.State) {
				if _, err := col.NewSTM(ctx, a.etcdClient, func(stm col.STM) error {
					jobs := a.jobs.ReadWrite(stm)
					var jobInfo pps.JobInfo
					if err := jobs.Get(jobID, &jobInfo); err != nil {
						return err
					}
					// We need to check again here because the job's state
					// might've changed since we first retrieved it
					if !jobStateToStopped(jobInfo.State) {
						jobInfo.State = pps.JobState_JOB_STOPPED
					}
					jobs.Put(jobID, &jobInfo)
					return nil
				}); err != nil {
					return nil, err
				}
			}
		}
	}

	if _, err := col.NewSTM(ctx, a.etcdClient, func(stm col.STM) error {
		return a.pipelines.ReadWrite(stm).Delete(request.Pipeline.Name)
	}); err != nil {
		return nil, err
	}
	return &types.Empty{}, nil
}

func (a *apiServer) StartPipeline(ctx context.Context, request *pps.StartPipelineRequest) (response *types.Empty, retErr error) {
	func() { a.Log(request, nil, nil, 0) }()
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())
	metricsFn := metrics.ReportUserAction(ctx, a.reporter, "StartPipeline")
	defer func(start time.Time) { metricsFn(start, retErr) }(time.Now())

	if err := a.updatePipelineState(ctx, request.Pipeline.Name, pps.PipelineState_PIPELINE_RUNNING); err != nil {
		return nil, err
	}
	return &types.Empty{}, nil
}

func (a *apiServer) StopPipeline(ctx context.Context, request *pps.StopPipelineRequest) (response *types.Empty, retErr error) {
	func() { a.Log(request, nil, nil, 0) }()
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())
	metricsFn := metrics.ReportUserAction(ctx, a.reporter, "StopPipeline")
	defer func(start time.Time) { metricsFn(start, retErr) }(time.Now())

	if err := a.updatePipelineState(ctx, request.Pipeline.Name, pps.PipelineState_PIPELINE_STOPPED); err != nil {
		return nil, err
	}
	return &types.Empty{}, nil
}

func (a *apiServer) RerunPipeline(ctx context.Context, request *pps.RerunPipelineRequest) (response *types.Empty, retErr error) {
	func() { a.Log(request, nil, nil, 0) }()
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())
	metricsFn := metrics.ReportUserAction(ctx, a.reporter, "RerunPipeline")
	defer func(start time.Time) { metricsFn(start, retErr) }(time.Now())

	return nil, fmt.Errorf("TODO")
}

func (a *apiServer) DeleteAll(ctx context.Context, request *types.Empty) (response *types.Empty, retErr error) {
	func() { a.Log(request, nil, nil, 0) }()
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())
	metricsFn := metrics.ReportUserAction(ctx, a.reporter, "PPSDeleteAll")
	defer func(start time.Time) { metricsFn(start, retErr) }(time.Now())

	pipelineInfos, err := a.ListPipeline(ctx, &pps.ListPipelineRequest{})
	if err != nil {
		return nil, err
	}

	for _, pipelineInfo := range pipelineInfos.PipelineInfo {
		if _, err := a.DeletePipeline(ctx, &pps.DeletePipelineRequest{
			Pipeline: pipelineInfo.Pipeline,
		}); err != nil {
			return nil, err
		}
	}

	jobInfos, err := a.ListJob(ctx, &pps.ListJobRequest{})
	if err != nil {
		return nil, err
	}

	for _, jobInfo := range jobInfos.JobInfo {
		if _, err := a.DeleteJob(ctx, &pps.DeleteJobRequest{jobInfo.Job}); err != nil {
			return nil, err
		}
	}

	return &types.Empty{}, err
}

func (a *apiServer) Version(version int64) error {
	a.versionLock.Lock()
	defer a.versionLock.Unlock()
	a.version = version
	return nil
}

func (a *apiServer) getPipelineCancel(pipelineName string) context.CancelFunc {
	a.pipelineCancelsLock.Lock()
	defer a.pipelineCancelsLock.Unlock()
	return a.pipelineCancels[pipelineName]
}

func (a *apiServer) deletePipelineCancel(pipelineName string) context.CancelFunc {
	a.pipelineCancelsLock.Lock()
	defer a.pipelineCancelsLock.Unlock()
	cancel := a.pipelineCancels[pipelineName]
	delete(a.pipelineCancels, pipelineName)
	return cancel
}

func (a *apiServer) setPipelineCancel(pipelineName string, cancel context.CancelFunc) {
	a.pipelineCancelsLock.Lock()
	defer a.pipelineCancelsLock.Unlock()
	a.pipelineCancels[pipelineName] = cancel
}

func (a *apiServer) getJobCancel(jobID string) context.CancelFunc {
	a.jobCancelsLock.Lock()
	defer a.jobCancelsLock.Unlock()
	return a.jobCancels[jobID]
}

func (a *apiServer) deleteJobCancel(jobID string) context.CancelFunc {
	a.jobCancelsLock.Lock()
	defer a.jobCancelsLock.Unlock()
	cancel := a.jobCancels[jobID]
	delete(a.jobCancels, jobID)
	return cancel
}

func (a *apiServer) setJobCancel(jobID string, cancel context.CancelFunc) {
	a.jobCancelsLock.Lock()
	defer a.jobCancelsLock.Unlock()
	a.jobCancels[jobID] = cancel
}

// pipelineWatcher watches for pipelines and launch pipelineManager
// when it gets a pipeline that falls into a shard assigned to the
// API server.
func (a *apiServer) pipelineWatcher(ctx context.Context, shard uint64) {
	b := backoff.NewInfiniteBackOff()
	backoff.RetryNotify(func() error {
		pipelineWatcher, err := a.pipelines.ReadOnly(ctx).WatchByIndex(stoppedIndex, false)
		if err != nil {
			return err
		}
		defer pipelineWatcher.Close()
		for {
			event, ok := <-pipelineWatcher.Watch()
			if !ok {
				return fmt.Errorf("pipelineWatcher closed unexpectedly")
			}
			if event.Err != nil {
				return event.Err
			}
			pipelineName := string(event.Key)
			if a.hasher.HashPipeline(pipelineName) != shard {
				// Skip pipelines that don't fall into my shard
				continue
			}
			switch event.Type {
			case watch.EventPut:
				var pipelineInfo pps.PipelineInfo
				if err := event.Unmarshal(&pipelineName, &pipelineInfo); err != nil {
					return err
				}
				if pipelineInfo.Input == nil {
					pipelineInfo.Input = translatePipelineInputs(pipelineInfo.Inputs)
				}
				if cancel := a.deletePipelineCancel(pipelineName); cancel != nil {
					protolion.Infof("Appear to be running a pipeline (%s) that's already being run; this may be a bug", pipelineName)
					protolion.Infof("cancelling pipeline: %s", pipelineName)
					cancel()
				}
				pipelineCtx, cancel := context.WithCancel(ctx)
				a.setPipelineCancel(pipelineName, cancel)
				protolion.Infof("launching pipeline manager for pipeline %s", pipelineInfo.Pipeline.Name)
				go a.pipelineManager(pipelineCtx, &pipelineInfo)
			case watch.EventDelete:
				if cancel := a.deletePipelineCancel(pipelineName); cancel != nil {
					protolion.Infof("cancelling pipeline: %s", pipelineName)
					cancel()
				}
			}
		}
	}, b, func(err error, d time.Duration) error {
		select {
		case <-ctx.Done():
			// Exit the retry loop if context got cancelled
			return err
		default:
		}
		protolion.Errorf("error receiving pipeline updates: %v; retrying in %v", err, d)
		return nil
	})
}

// jobWatcher watches for unfinished jobs and launches jobManagers for
// the jobs that fall into this server's shards
func (a *apiServer) jobWatcher(ctx context.Context, shard uint64) {
	b := backoff.NewInfiniteBackOff()
	backoff.RetryNotify(func() error {
		// Wait for job events where JobInfo.Stopped is set to "false", and then
		// start JobManagers for those jobs
		jobWatcher, err := a.jobs.ReadOnly(ctx).WatchByIndex(stoppedIndex, false)
		if err != nil {
			return err
		}
		defer jobWatcher.Close()
		for {
			event, ok := <-jobWatcher.Watch()
			if !ok {
				return fmt.Errorf("jobWatcher closed unexpectedly")
			}
			if event.Err != nil {
				return event.Err
			}
			jobID := string(event.Key)
			if a.hasher.HashJob(jobID) != shard {
				// Skip jobs that don't fall into my shard
				continue
			}
			switch event.Type {
			case watch.EventPut:
				var jobInfo pps.JobInfo
				if err := event.Unmarshal(&jobID, &jobInfo); err != nil {
					return err
				}
				if cancel := a.deleteJobCancel(jobID); cancel != nil {
					protolion.Infof("Appear to be running a job (%s) that's already being run; this may be a bug", jobID)
					protolion.Infof("cancelling job: %s", jobID)
					cancel()
				}
				jobCtx, cancel := context.WithCancel(ctx)
				a.setJobCancel(jobID, cancel)
				protolion.Infof("launching job manager for job %s", jobInfo.Job.ID)
				go a.jobManager(jobCtx, &jobInfo)
			case watch.EventDelete:
				if cancel := a.deleteJobCancel(jobID); cancel != nil {
					cancel()
					protolion.Infof("cancelling job: %s", jobID)
				}
			}
		}
	}, b, func(err error, d time.Duration) error {
		select {
		case <-ctx.Done():
			// Exit the retry loop if context got cancelled
			return err
		default:
		}
		protolion.Errorf("error receiving job updates: %v; retrying in %v", err, d)
		return nil
	})
}

func isAlreadyExistsErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "already exists")
}

func isNotFoundErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not found")
}

func (a *apiServer) getRunningJobsForPipeline(ctx context.Context, pipelineInfo *pps.PipelineInfo) ([]*pps.JobInfo, error) {
	iter, err := a.jobs.ReadOnly(ctx).GetByIndex(stoppedIndex, false)
	if err != nil {
		return nil, err
	}
	var jobInfos []*pps.JobInfo
	for {
		var jobID string
		var jobInfo pps.JobInfo
		ok, err := iter.Next(&jobID, &jobInfo)
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		if jobInfo.PipelineID == pipelineInfo.ID {
			jobInfos = append(jobInfos, &jobInfo)
		}
	}
	return jobInfos, nil
}

// watchJobCompletion waits for a job to complete and then sends the job back on jobCompletionCh.
func (a *apiServer) watchJobCompletion(ctx context.Context, job *pps.Job, jobCompletionCh chan *pps.Job) {
	b := backoff.NewInfiniteBackOff()
	backoff.RetryNotify(func() error {
		if _, err := a.InspectJob(ctx, &pps.InspectJobRequest{
			Job:        job,
			BlockState: true,
		}); err != nil {
			// If a job has been deleted, it's also "completed" for
			// our purposes.
			if strings.Contains(err.Error(), "deleted") {
				select {
				case <-ctx.Done():
				case jobCompletionCh <- job:
				}
				return nil
			}
			return err
		}
		select {
		case <-ctx.Done():
		case jobCompletionCh <- job:
		}
		return nil
	}, b, func(err error, d time.Duration) error {
		select {
		case <-ctx.Done():
			// Exit the retry loop if context got cancelled
			return err
		default:
		}
		return nil
	})
}

func (a *apiServer) scaleDownWorkers(ctx context.Context, rcName string) error {
	rc := a.kubeClient.ReplicationControllers(a.namespace)
	workerRc, err := rc.Get(rcName)
	if err != nil {
		return err
	}
	workerRc.Spec.Replicas = 0
	_, err = rc.Update(workerRc)
	return err
}

func (a *apiServer) scaleUpWorkers(ctx context.Context, rcName string, parallelismSpec *pps.ParallelismSpec) error {
	rc := a.kubeClient.ReplicationControllers(a.namespace)
	workerRc, err := rc.Get(rcName)
	if err != nil {
		return err
	}
	parallelism, err := GetExpectedNumWorkers(a.kubeClient, parallelismSpec)
	if err != nil {
		return err
	}
	workerRc.Spec.Replicas = int32(parallelism)
	_, err = rc.Update(workerRc)
	return err
}

func (a *apiServer) pipelineManager(ctx context.Context, pipelineInfo *pps.PipelineInfo) {
	// Clean up workers if the pipeline gets cancelled
	pipelineName := pipelineInfo.Pipeline.Name
	go func() {
		// Clean up workers if the pipeline gets cancelled
		<-ctx.Done()
		rcName := PipelineRcName(pipelineInfo.Pipeline.Name, pipelineInfo.Version)
		if err := a.deleteWorkers(rcName); err != nil {
			protolion.Errorf("error deleting workers for pipeline: %v", pipelineName)
		}
		protolion.Infof("deleted workers for pipeline: %v", pipelineName)
	}()

	b := backoff.NewInfiniteBackOff()
	backoff.RetryNotify(func() error {
		// We use a new context for this particular instance of the retry
		// loop, to ensure that all resources are released properly when
		// this job retries.
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		if err := a.updatePipelineState(ctx, pipelineName, pps.PipelineState_PIPELINE_RUNNING); err != nil {
			return err
		}

		var provenance []*pfs.Repo
		for _, commit := range inputCommits(pipelineInfo.Input) {
			provenance = append(provenance, commit.Repo)
		}

		pfsClient, err := a.getPFSClient()
		if err != nil {
			return err
		}
		// Create the output repo; if it already exists, do nothing
		if _, err := pfsClient.CreateRepo(ctx, &pfs.CreateRepoRequest{
			Repo:       &pfs.Repo{pipelineName},
			Provenance: provenance,
		}); err != nil {
			if !isAlreadyExistsErr(err) {
				return err
			}
		}

		// Create a k8s replication controller that runs the workers
		if err := a.createWorkersForPipeline(pipelineInfo); err != nil {
			if !isAlreadyExistsErr(err) {
				return err
			}
		}

		branchSetFactory, err := newBranchSetFactory(ctx, pfsClient, pipelineInfo.Input)
		if err != nil {
			return err
		}
		defer branchSetFactory.Close()

		runningJobList, err := a.getRunningJobsForPipeline(ctx, pipelineInfo)
		if err != nil {
			return err
		}

		jobCompletionCh := make(chan *pps.Job)
		runningJobSet := make(map[string]bool)
		for _, job := range runningJobList {
			go a.watchJobCompletion(ctx, job.Job, jobCompletionCh)
			runningJobSet[job.Job.ID] = true
		}
		// If there's currently no running jobs, we want to trigger
		// the code that sets the timer for scale-down.
		if len(runningJobList) == 0 {
			go func() {
				select {
				case jobCompletionCh <- &pps.Job{}:
				case <-ctx.Done():
				}
			}()
		}

		scaleDownCh := make(chan struct{})
		var scaleDownTimer *time.Timer
		var job *pps.Job
	nextInput:
		for {
			var branchSet *branchSet
			select {
			case branchSet = <-branchSetFactory.Chan():
			case completedJob := <-jobCompletionCh:
				delete(runningJobSet, completedJob.ID)
				if len(runningJobSet) == 0 {
					// If the scaleDownThreshold is nil, we interpret it
					// as "no scale down".  We then use a threshold of
					// (practically) infinity.  This practically disables
					// the feature, without requiring us to write two code
					// paths.
					var scaleDownThreshold time.Duration
					if pipelineInfo.ScaleDownThreshold != nil {
						scaleDownThreshold, err = types.DurationFromProto(pipelineInfo.ScaleDownThreshold)
						if err != nil {
							return err
						}
					} else {
						scaleDownThreshold = time.Duration(math.MaxInt64)
					}
					// We want the timer goro's lifetime to be tied to
					// the lifetime of this particular run of the retry
					// loop
					ctx, cancel := context.WithCancel(ctx)
					defer cancel()
					scaleDownTimer = time.AfterFunc(scaleDownThreshold, func() {
						// We want the scaledown to happen synchronously
						// in pipelineManager, as opposed to asynchronously
						// in a separate goroutine, in other to prevent
						// potential races.
						select {
						case <-ctx.Done():
						case scaleDownCh <- struct{}{}:
						}
					})
				}
				continue nextInput
			case <-scaleDownCh:
				// We need to check if there's indeed no running job,
				// because it might happen that the timer expired while
				// we were creating a job.
				if len(runningJobSet) == 0 {
					if err := a.scaleDownWorkers(ctx, PipelineRcName(pipelineInfo.Pipeline.Name, pipelineInfo.Version)); err != nil {
						return err
					}
				}
				continue nextInput
			}
			if branchSet.Err != nil {
				return err
			}

			// (create JobInput for new processing job)
			jobInput := proto.Clone(pipelineInfo.Input).(*pps.Input)
			visit(jobInput, func(input *pps.Input) {
				if input.Atom != nil {
					for _, branch := range branchSet.Branches {
						if input.Atom.Commit.Repo.Name == branch.Head.Repo.Name && input.Atom.Commit.ID == branch.Name {
							input.Atom.Commit.ID = branch.Head.ID
						}
					}
					input.Atom.FromCommitID = ""
				}
			})

			// Check if this input set has already been processed
			jobIter, err := a.jobs.ReadOnly(ctx).GetByIndex(jobsInputIndex, jobInput)
			if err != nil {
				return err
			}

			// Check if any of the jobs in jobIter have been run already. If so, skip
			// this input.
			for {
				var jobID string
				var jobInfo pps.JobInfo
				ok, err := jobIter.Next(&jobID, &jobInfo)
				if !ok {
					break
				}
				if err != nil {
					return err
				}
				if jobInfo.PipelineID == pipelineInfo.ID && jobInfo.PipelineVersion == pipelineInfo.Version {
					// TODO(derek): we should check if the output commit exists.  If the
					// output commit has been deleted, we should re-run the job.
					continue nextInput
				}
			}

			job, err = a.CreateJob(ctx, &pps.CreateJobRequest{
				Pipeline: pipelineInfo.Pipeline,
				Input:    jobInput,
				// TODO(derek): Note that once the pipeline restarts, the `job`
				// variable is lost and we don't know who is our parent job.
				ParentJob: job,
			})
			if err != nil {
				return err
			}
			if scaleDownTimer != nil {
				scaleDownTimer.Stop()
			}
			runningJobSet[job.ID] = true
			go a.watchJobCompletion(ctx, job, jobCompletionCh)
			protolion.Infof("pipeline %s created job %v with the following input: %v", pipelineName, job.ID, jobInput)
		}
		panic("unreachable")
		return nil
	}, b, func(err error, d time.Duration) error {
		select {
		case <-ctx.Done():
			// Exit the retry loop if context got cancelled
			return err
		default:
		}
		protolion.Errorf("error running pipelineManager for pipeline %s: %v; retrying in %v", pipelineInfo.Pipeline.Name, err, d)
		if err := a.updatePipelineState(ctx, pipelineName, pps.PipelineState_PIPELINE_RESTARTING); err != nil {
			protolion.Errorf("error updating pipeline state: %v", err)
		}
		return nil
	})
}

// pipelineStateToStopped defines what pipeline states are "stopped"
// states, meaning that pipelines in this state should not be managed
// by pipelineManager
func pipelineStateToStopped(state pps.PipelineState) bool {
	switch state {
	case pps.PipelineState_PIPELINE_STARTING:
		return false
	case pps.PipelineState_PIPELINE_RUNNING:
		return false
	case pps.PipelineState_PIPELINE_RESTARTING:
		return false
	case pps.PipelineState_PIPELINE_STOPPED:
		return true
	case pps.PipelineState_PIPELINE_FAILURE:
		return true
	default:
		panic(fmt.Sprintf("unrecognized pipeline state: %s", state))
	}
}

func (a *apiServer) updatePipelineState(ctx context.Context, pipelineName string, state pps.PipelineState) error {
	_, err := col.NewSTM(ctx, a.etcdClient, func(stm col.STM) error {
		pipelines := a.pipelines.ReadWrite(stm)
		pipelineInfo := new(pps.PipelineInfo)
		if err := pipelines.Get(pipelineName, pipelineInfo); err != nil {
			return err
		}
		pipelineInfo.State = state
		pipelineInfo.Stopped = pipelineStateToStopped(state)
		pipelines.Put(pipelineName, pipelineInfo)
		return nil
	})
	if isNotFoundErr(err) {
		return newErrPipelineNotFound(pipelineName)
	}
	return err
}

func (a *apiServer) updateJobState(stm col.STM, jobInfo *pps.JobInfo, state pps.JobState) error {
	// Update job counts
	if jobInfo.Pipeline != nil {
		pipelines := a.pipelines.ReadWrite(stm)
		pipelineInfo := new(pps.PipelineInfo)
		if err := pipelines.Get(jobInfo.Pipeline.Name, pipelineInfo); err != nil {
			return err
		}
		if pipelineInfo.JobCounts == nil {
			pipelineInfo.JobCounts = make(map[int32]int32)
		}
		if pipelineInfo.JobCounts[int32(jobInfo.State)] != 0 {
			pipelineInfo.JobCounts[int32(jobInfo.State)]--
		}
		pipelineInfo.JobCounts[int32(state)]++
		pipelines.Put(pipelineInfo.Pipeline.Name, pipelineInfo)
	}
	jobInfo.State = state
	jobInfo.Stopped = jobStateToStopped(state)
	jobs := a.jobs.ReadWrite(stm)
	jobs.Put(jobInfo.Job.ID, jobInfo)
	return nil
}

func (a *apiServer) jobManager(ctx context.Context, jobInfo *pps.JobInfo) {
	jobID := jobInfo.Job.ID
	b := backoff.NewInfiniteBackOff()
	backoff.RetryNotify(func() error {
		// We use a new context for this particular instance of the retry
		// loop, to ensure that all resources are released properly when
		// this job retries.
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		if jobInfo.ParentJob != nil {
			// Wait for the parent job to finish, to ensure that output
			// commits are ordered correctly, and that this job doesn't
			// contend for workers with its parent.
			if _, err := a.InspectJob(ctx, &pps.InspectJobRequest{
				Job:        jobInfo.ParentJob,
				BlockState: true,
			}); err != nil {
				return err
			}
		}

		pfsClient, err := a.getPFSClient()
		if err != nil {
			return err
		}

		// Create workers and output repo if 'jobInfo' belongs to an orphan job
		if jobInfo.Pipeline == nil {
			// Create output repo for this job
			var provenance []*pfs.Repo
			visit(jobInfo.Input, func(input *pps.Input) {
				if input.Atom != nil {
					provenance = append(provenance, input.Atom.Commit.Repo)
				}
			})
			if _, err := pfsClient.CreateRepo(ctx, &pfs.CreateRepoRequest{
				Repo:       jobInfo.OutputRepo,
				Provenance: provenance,
			}); err != nil {
				// (if output repo already exists, do nothing)
				if !isAlreadyExistsErr(err) {
					return err
				}
			}

			// Create workers in kubernetes (i.e. create replication controller)
			if err := a.createWorkersForOrphanJob(jobInfo); err != nil {
				if !isAlreadyExistsErr(err) {
					return err
				}
			}
			go func() {
				// Clean up workers if the job gets cancelled
				<-ctx.Done()
				rcName := JobRcName(jobInfo.Job.ID)
				if err := a.deleteWorkers(rcName); err != nil {
					protolion.Errorf("error deleting workers for job: %v", jobID)
				}
				protolion.Infof("deleted workers for job: %v", jobID)
			}()
		}

		// Set the state of this job to 'RUNNING'
		_, err = col.NewSTM(ctx, a.etcdClient, func(stm col.STM) error {
			jobs := a.jobs.ReadWrite(stm)
			jobInfo := new(pps.JobInfo)
			if err := jobs.Get(jobID, jobInfo); err != nil {
				return err
			}
			return a.updateJobState(stm, jobInfo, pps.JobState_JOB_RUNNING)
		})
		if err != nil {
			return err
		}

		// Start worker pool
		var wp WorkerPool
		if jobInfo.Pipeline != nil {
			// We scale up the workers before we run a job, to ensure
			// that the job will have workers to use.  Note that scaling
			// a RC is idempotent: nothing happens if the workers have
			// already been scaled.
			rcName := PipelineRcName(jobInfo.Pipeline.Name, jobInfo.PipelineVersion)
			if err := a.scaleUpWorkers(ctx, rcName, jobInfo.ParallelismSpec); err != nil {
				return err
			}
			wp, err = a.newWorkerPool(ctx, rcName, jobInfo.Job.ID)
			if err != nil {
				return err
			}
		} else {
			wp, err = a.newWorkerPool(ctx, JobRcName(jobInfo.Job.ID), jobInfo.Job.ID)
			if err != nil {
				return err
			}
		}

		// We have a goroutine that receives the datums that fail to
		// be processed, and put them back onto the datum queue.
		jobFailedCh := make(chan struct{})
		go func() {
			var dts []*datum
			for {
				if len(dts) > 0 {
					select {
					case wp.DataCh() <- dts[0]:
						protolion.Infof("retrying datum %v", dts[0].inputs)
						dts = dts[1:]
					case dt := <-wp.FailCh():
						dt.retries++
						if dt.retries >= MaximumRetriesPerDatum {
							close(jobFailedCh)
							return
						}
						protolion.Infof("datum %v is queued up for retry", dt.inputs)
						dts = append(dts, dt)
					case <-ctx.Done():
						return
					}
				} else {
					select {
					case dt := <-wp.FailCh():
						dt.retries++
						if dt.retries >= MaximumRetriesPerDatum {
							close(jobFailedCh)
							return
						}
						protolion.Infof("datum %v is queued up for retry", dt.inputs)
						dts = append(dts, dt)
					case <-ctx.Done():
						return
					}
				}
			}
		}()

		// process all datums
		df, err := newDatumFactory(ctx, pfsClient, jobInfo.Input)
		if err != nil {
			return err
		}
		var inflightData int64
		var processedData int64
		var etcdProcessedData int64 // the value of processedData we've sent to etcd
		totalData := int64(df.Len())
		// This goro is responsible for updating job progress
		// The values sent on this channel will be the number of datums
		// processed so far.
		progressCh := make(chan int64)
		go func() {
			var processed int64
			for {
				// We do the first update before receiving a progress,
				// because we want to update the total number of datums.
				if _, err := col.NewSTM(ctx, a.etcdClient, func(stm col.STM) error {
					jobs := a.jobs.ReadWrite(stm)
					jobInfo := new(pps.JobInfo)
					if err := jobs.Get(jobID, jobInfo); err != nil {
						return err
					}
					// In case this goro races with the goro that sets
					// this job as success, we don't want to overwrite
					// the completed progress bar.
					if jobInfo.DataProcessed != totalData {
						jobInfo.DataProcessed = processed
					}
					jobInfo.DataTotal = int64(totalData)
					jobs.Put(jobInfo.Job.ID, jobInfo)
					return nil
				}); err != nil {
					protolion.Errorf("error updating job progress: %+v", err)
				}
				select {
				case processed = <-progressCh:
				case <-ctx.Done():
					// exit when the jobManager exits
					return
				}
			}
		}()

		tree := hashtree.NewHashTree()
		i := 0
		input := df.Datum(i)
		i++
		for {
			var resp hashtree.HashTree
			var failed bool
			if input != nil {
				select {
				case wp.DataCh() <- &datum{
					inputs: input,
				}:
					input = df.Datum(i)
					i++
				case resp = <-wp.SuccessCh():
					inflightData--
				case <-jobFailedCh:
					failed = true
					inflightData--
				}
			} else {
				if inflightData == 0 {
					break
				}
				select {
				case resp = <-wp.SuccessCh():
				case <-jobFailedCh:
					failed = true
				}
				inflightData--
			}
			if resp != nil {
				if err := tree.Merge(resp); err != nil {
					return err
				}
				processedData++
				// so as not to overwhelm etcd we update at most 100 times per job
				if (float64(processedData-etcdProcessedData) / float64(totalData)) > .01 {
					etcdProcessedData = processedData
					// If we fail to send the progress, so be it.
					// We don't want to slow down the distribution of datums
					// in order to update progress.
					select {
					case progressCh <- processedData:
					default:
					}
				}
			}
			if failed {
				_, err = col.NewSTM(ctx, a.etcdClient, func(stm col.STM) error {
					jobs := a.jobs.ReadWrite(stm)
					jobInfo := new(pps.JobInfo)
					if err := jobs.Get(jobID, jobInfo); err != nil {
						return err
					}
					jobInfo.Finished = now()
					return a.updateJobState(stm, jobInfo, pps.JobState_JOB_FAILURE)
				})
				return err
			}
		}

		finishedTree, err := tree.Finish()
		if err != nil {
			return err
		}

		data, err := hashtree.Serialize(finishedTree)
		if err != nil {
			return err
		}

		objClient, err := a.getObjectClient()
		if err != nil {
			return err
		}

		putObjClient, err := objClient.PutObject(ctx)
		if err != nil {
			return err
		}
		if err := putObjClient.Send(&pfs.PutObjectRequest{
			Value: data,
		}); err != nil {
			return err
		}
		object, err := putObjClient.CloseAndRecv()
		if err != nil {
			return err
		}

		var provenance []*pfs.Commit
		for _, input := range jobInfo.Inputs {
			provenance = append(provenance, input.Commit)
		}

		outputCommit, err := pfsClient.BuildCommit(ctx, &pfs.BuildCommitRequest{
			Parent: &pfs.Commit{
				Repo: jobInfo.OutputRepo,
			},
			Branch:     jobInfo.OutputBranch,
			Provenance: provenance,
			Tree:       object,
		})
		if err != nil {
			return err
		}

		if jobInfo.Egress != nil {
			objClient, err := obj.NewClientFromURLAndSecret(ctx, jobInfo.Egress.URL)
			if err != nil {
				return err
			}
			url, err := url.Parse(jobInfo.Egress.URL)
			if err != nil {
				return err
			}
			client := client.APIClient{
				PfsAPIClient: pfsClient,
			}
			client.SetMaxConcurrentStreams(100)
			if err := pfs_sync.PushObj(client, outputCommit, objClient, strings.TrimPrefix(url.Path, "/")); err != nil {
				return err
			}
		}

		// Record the job's output commit and 'Finished' timestamp, and mark the job
		// as a SUCCESS
		_, err = col.NewSTM(ctx, a.etcdClient, func(stm col.STM) error {
			jobs := a.jobs.ReadWrite(stm)
			jobInfo := new(pps.JobInfo)
			if err := jobs.Get(jobID, jobInfo); err != nil {
				return err
			}
			jobInfo.OutputCommit = outputCommit
			jobInfo.Finished = now()
			// By definition, we will have processed all datums at this point
			jobInfo.DataProcessed = totalData
			return a.updateJobState(stm, jobInfo, pps.JobState_JOB_SUCCESS)
		})
		return err
	}, b, func(err error, d time.Duration) error {
		select {
		case <-ctx.Done():
			// Exit the retry loop if context got cancelled
			return err
		default:
		}

		protolion.Errorf("error running jobManager for job %s: %v; retrying in %v", jobInfo.Job.ID, err, d)

		// Increment the job's restart count
		_, err = col.NewSTM(ctx, a.etcdClient, func(stm col.STM) error {
			jobs := a.jobs.ReadWrite(stm)
			jobInfo := new(pps.JobInfo)
			if err := jobs.Get(jobID, jobInfo); err != nil {
				return err
			}
			jobInfo.Restart++
			jobs.Put(jobInfo.Job.ID, jobInfo)
			return nil
		})
		if err != nil {
			protolion.Errorf("error incrementing job %s's restart count", jobInfo.Job.ID)
		}

		return nil
	})
}

// jobStateToStopped defines what job states are "stopped" states,
// meaning that jobs in this state should not be managed by jobManager
func jobStateToStopped(state pps.JobState) bool {
	switch state {
	case pps.JobState_JOB_STARTING:
		return false
	case pps.JobState_JOB_RUNNING:
		return false
	case pps.JobState_JOB_SUCCESS:
		return true
	case pps.JobState_JOB_FAILURE:
		return true
	case pps.JobState_JOB_STOPPED:
		return true
	default:
		panic(fmt.Sprintf("unrecognized job state: %s", state))
	}
}

func (a *apiServer) createWorkersForOrphanJob(jobInfo *pps.JobInfo) error {
	parallelism, err := GetExpectedNumWorkers(a.kubeClient, jobInfo.ParallelismSpec)
	if err != nil {
		return err
	}
	options := a.getWorkerOptions(
		JobRcName(jobInfo.Job.ID),
		int32(parallelism),
		jobInfo.Transform)
	// Set the job name env
	options.workerEnv = append(options.workerEnv, api.EnvVar{
		Name:  client.PPSJobIDEnv,
		Value: jobInfo.Job.ID,
	})
	return a.createWorkerRc(options)
}

func (a *apiServer) createWorkersForPipeline(pipelineInfo *pps.PipelineInfo) error {
	parallelism, err := GetExpectedNumWorkers(a.kubeClient, pipelineInfo.ParallelismSpec)
	if err != nil {
		return err
	}
	options := a.getWorkerOptions(
		PipelineRcName(pipelineInfo.Pipeline.Name, pipelineInfo.Version),
		int32(parallelism),
		pipelineInfo.Transform)
	// Set the pipeline name env
	options.workerEnv = append(options.workerEnv, api.EnvVar{
		Name:  client.PPSPipelineNameEnv,
		Value: pipelineInfo.Pipeline.Name,
	})
	return a.createWorkerRc(options)
}

func (a *apiServer) deleteWorkersForPipeline(pipelineInfo *pps.PipelineInfo) error {
	falseVal := false
	deleteOptions := &api.DeleteOptions{
		OrphanDependents: &falseVal,
	}
	rcName := PipelineRcName(pipelineInfo.Pipeline.Name, pipelineInfo.Version)
	return a.kubeClient.ReplicationControllers(a.namespace).Delete(rcName, deleteOptions)
}

func (a *apiServer) deleteWorkers(rcName string) error {
	falseVal := false
	deleteOptions := &api.DeleteOptions{
		OrphanDependents: &falseVal,
	}
	return a.kubeClient.ReplicationControllers(a.namespace).Delete(rcName, deleteOptions)
}

func (a *apiServer) AddShard(shard uint64) error {
	ctx, cancel := context.WithCancel(context.Background())
	a.shardLock.Lock()
	defer a.shardLock.Unlock()
	if _, ok := a.shardCtxs[shard]; ok {
		return fmt.Errorf("shard %d is being added twice; this is likely a bug", shard)
	}
	a.shardCtxs[shard] = &ctxAndCancel{
		ctx:    ctx,
		cancel: cancel,
	}
	protolion.Infof("adding shard %d", shard)
	go a.jobWatcher(ctx, shard)
	go a.pipelineWatcher(ctx, shard)
	return nil
}

func (a *apiServer) DeleteShard(shard uint64) error {
	a.shardLock.Lock()
	defer a.shardLock.Unlock()
	ctxAndCancel, ok := a.shardCtxs[shard]
	if !ok {
		return fmt.Errorf("shard %d is being deleted, but it was never added; this is likely a bug", shard)
	}
	ctxAndCancel.cancel()
	delete(a.shardCtxs, shard)
	protolion.Infof("removing shard %d", shard)
	return nil
}

func (a *apiServer) getPFSClient() (pfs.APIClient, error) {
	if a.pachConn == nil {
		var onceErr error
		a.pachConnOnce.Do(func() {
			pachConn, err := grpc.Dial(a.address, grpc.WithInsecure())
			if err != nil {
				onceErr = err
			}
			a.pachConn = pachConn
		})
		if onceErr != nil {
			return nil, onceErr
		}
	}
	return pfs.NewAPIClient(a.pachConn), nil
}

func (a *apiServer) getObjectClient() (pfs.ObjectAPIClient, error) {
	if a.pachConn == nil {
		var onceErr error
		a.pachConnOnce.Do(func() {
			pachConn, err := grpc.Dial(a.address, grpc.WithInsecure())
			if err != nil {
				onceErr = err
			}
			a.pachConn = pachConn
		})
		if onceErr != nil {
			return nil, onceErr
		}
	}
	return pfs.NewObjectAPIClient(a.pachConn), nil
}

// RepoNameToEnvString is a helper which uppercases a repo name for
// use in environment variable names.
func RepoNameToEnvString(repoName string) string {
	return strings.ToUpper(repoName)
}

func (a *apiServer) rcPods(rcName string) ([]api.Pod, error) {
	podList, err := a.kubeClient.Pods(a.namespace).List(api.ListOptions{
		TypeMeta: unversioned.TypeMeta{
			Kind:       "ListOptions",
			APIVersion: "v1",
		},
		LabelSelector: kube_labels.SelectorFromSet(labels(rcName)),
	})
	if err != nil {
		return nil, err
	}
	return podList.Items, nil
}

func labels(app string) map[string]string {
	return map[string]string{
		"app":   app,
		"suite": suite,
	}
}

type podSlice []api.Pod

func (s podSlice) Len() int {
	return len(s)
}
func (s podSlice) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}
func (s podSlice) Less(i, j int) bool {
	return s[i].ObjectMeta.Name < s[j].ObjectMeta.Name
}

func now() *types.Timestamp {
	t, err := types.TimestampProto(time.Now())
	if err != nil {
		panic(err)
	}
	return t
}
