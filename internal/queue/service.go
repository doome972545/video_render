package queue

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"videoremix/internal/download"
	"videoremix/internal/recipe"
)

// --- Ports ---

// JobStore is durable persistence of Job state, enabling resume after restart.
type JobStore interface {
	Save(job Job) error
	Get(id JobID) (Job, error)
	ListByState(state State) ([]Job, error)
	ListByBatch(batch BatchID) ([]Job, error)
}

// WorkerDispatcher delegates the actual render for a Job.
type WorkerDispatcher interface {
	Dispatch(job Job) (RenderResult, error)
}

// ProgressReporter emits Job/batch progress events.
type ProgressReporter interface {
	Emit(ev ProgressEvent)
	Subscribe(batch BatchID) (<-chan ProgressEvent, error)
}

// ErrorClassifier decides whether a render error is retryable or terminal.
type ErrorClassifier func(err error) (retryable bool)

// DefaultErrorClassifier treats download.ErrPermanent as terminal, else retry.
func DefaultErrorClassifier(err error) bool {
	return !isPermanent(err)
}

func isPermanent(err error) bool {
	// Import-light check: match on the download sentinel error text.
	return err != nil && (containsErr(err, download.ErrPermanent) || containsErr(err, download.ErrUnsupported))
}

func containsErr(err, target error) bool {
	for e := err; e != nil; {
		if e == target {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := e.(unwrapper)
		if !ok {
			break
		}
		e = u.Unwrap()
	}
	return false
}

// --- Service ---

// Service is the QueueService implementation with a worker pool.
type Service struct {
	store       JobStore
	dispatcher  WorkerDispatcher
	reporter    ProgressReporter
	retry       RetryPolicy
	classify    ErrorClassifier
	concurrency int

	pending chan JobID
	cancels sync.Map // JobID -> struct{} and BatchID -> struct{}
	wg      sync.WaitGroup
	once    sync.Once
	ctx     context.Context
	cancel  context.CancelFunc
	seq     int
	seqMu   sync.Mutex
}

// NewService constructs the queue and starts its worker pool.
func NewService(store JobStore, dispatcher WorkerDispatcher, reporter ProgressReporter, retry RetryPolicy, concurrency int) *Service {
	if concurrency <= 0 {
		concurrency = 4
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{
		store:       store,
		dispatcher:  dispatcher,
		reporter:    reporter,
		retry:       retry,
		classify:    DefaultErrorClassifier,
		concurrency: concurrency,
		pending:     make(chan JobID, 1024),
		ctx:         ctx,
		cancel:      cancel,
	}
	s.start()
	return s
}

func (s *Service) start() {
	s.once.Do(func() {
		for i := 0; i < s.concurrency; i++ {
			s.wg.Add(1)
			go s.worker()
		}
	})
}

// EnqueueBatch enqueues one Job per Recipe under a new BatchID.
func (s *Service) EnqueueBatch(recipes []recipe.Recipe, priority Priority) (BatchID, error) {
	batch := BatchID(fmt.Sprintf("batch-%d", time.Now().UnixNano()))
	for _, r := range recipes {
		if _, err := s.enqueue(batch, r.ID, priority); err != nil {
			return batch, err
		}
	}
	return batch, nil
}

// Enqueue adds a single Job.
func (s *Service) Enqueue(recipeID recipe.RecipeID, priority Priority) (JobID, error) {
	return s.enqueue("", recipeID, priority)
}

func (s *Service) enqueue(batch BatchID, recipeID recipe.RecipeID, priority Priority) (JobID, error) {
	id := s.nextID()
	job := Job{
		ID:        id,
		BatchID:   batch,
		RecipeID:  recipeID,
		Priority:  priority,
		State:     StatePending,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := s.store.Save(job); err != nil {
		return "", fmt.Errorf("queue: persist: %w", err)
	}
	s.report(job, "")
	s.pending <- id
	return id, nil
}

func (s *Service) nextID() JobID {
	s.seqMu.Lock()
	defer s.seqMu.Unlock()
	s.seq++
	return JobID(fmt.Sprintf("job-%d-%d", time.Now().UnixNano(), s.seq))
}

// Cancel marks a job cancelled cooperatively.
func (s *Service) Cancel(jobID JobID) error {
	s.cancels.Store("job:"+string(jobID), struct{}{})
	job, err := s.store.Get(jobID)
	if err != nil {
		return err
	}
	if job.State == StatePending {
		job.State = StateCancelled
		job.UpdatedAt = time.Now()
		_ = s.store.Save(job)
		s.report(job, "")
	}
	return nil
}

// CancelBatch cancels every job in a batch.
func (s *Service) CancelBatch(batch BatchID) error {
	s.cancels.Store("batch:"+string(batch), struct{}{})
	jobs, err := s.store.ListByBatch(batch)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		_ = s.Cancel(j.ID)
	}
	return nil
}

// Status returns a job's current state.
func (s *Service) Status(jobID JobID) (State, error) {
	j, err := s.store.Get(jobID)
	if err != nil {
		return "", err
	}
	return j.State, nil
}

// BatchProgress aggregates a snapshot for a batch.
func (s *Service) BatchProgress(batch BatchID) (ProgressSnapshot, error) {
	jobs, err := s.store.ListByBatch(batch)
	if err != nil {
		return ProgressSnapshot{}, err
	}
	snap := ProgressSnapshot{BatchID: batch, Total: len(jobs)}
	for _, j := range jobs {
		switch j.State {
		case StatePending:
			snap.Pending++
		case StateRunning:
			snap.Running++
		case StateCompleted:
			snap.Completed++
		case StateFailed, StateDeadLetter:
			snap.Failed++
		case StateCancelled:
			snap.Cancelled++
		}
	}
	return snap, nil
}

// Subscribe proxies to the reporter for batch progress events.
func (s *Service) Subscribe(batch BatchID) (<-chan ProgressEvent, error) {
	return s.reporter.Subscribe(batch)
}

// Shutdown stops workers after draining in-flight jobs.
func (s *Service) Shutdown() {
	s.cancel()
	close(s.pending)
	s.wg.Wait()
}

func (s *Service) worker() {
	defer s.wg.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		case id, ok := <-s.pending:
			if !ok {
				return
			}
			s.process(id)
		}
	}
}

func (s *Service) process(id JobID) {
	job, err := s.store.Get(id)
	if err != nil {
		return
	}
	if s.isCancelled(job) {
		job.State = StateCancelled
		job.UpdatedAt = time.Now()
		_ = s.store.Save(job)
		s.report(job, "")
		return
	}

	job.State = StateRunning
	job.UpdatedAt = time.Now()
	_ = s.store.Save(job)
	s.report(job, "")

	result, rerr := s.dispatcher.Dispatch(job)
	if rerr == nil {
		job.State = StateCompleted
		job.OutputRef = result.OutputPath
		job.UpdatedAt = time.Now()
		_ = s.store.Save(job)
		s.report(job, "")
		return
	}

	// Failure handling with retry/backoff and dead-lettering.
	job.LastError = rerr.Error()
	if s.classify(rerr) && job.RetryCount < s.retry.MaxRetries {
		job.RetryCount++
		job.State = StatePending
		job.UpdatedAt = time.Now()
		_ = s.store.Save(job)
		s.report(job, rerr.Error())
		delay := s.retry.Backoff(job.RetryCount)
		go func(id JobID, d time.Duration) {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-s.ctx.Done():
			case <-t.C:
				select {
				case s.pending <- id:
				case <-s.ctx.Done():
				}
			}
		}(id, delay)
		return
	}

	job.State = StateDeadLetter
	job.UpdatedAt = time.Now()
	_ = s.store.Save(job)
	s.report(job, rerr.Error())
}

func (s *Service) isCancelled(job Job) bool {
	if _, ok := s.cancels.Load("job:" + string(job.ID)); ok {
		return true
	}
	if job.BatchID != "" {
		if _, ok := s.cancels.Load("batch:" + string(job.BatchID)); ok {
			return true
		}
	}
	return false
}

func (s *Service) report(job Job, errMsg string) {
	if s.reporter == nil {
		return
	}
	s.reporter.Emit(ProgressEvent{
		JobID:   job.ID,
		BatchID: job.BatchID,
		State:   job.State,
		Error:   errMsg,
	})
}

// --- MemoryJobStore adapter ---

type MemoryJobStore struct {
	mu sync.RWMutex
	m  map[JobID]Job
}

func NewMemoryJobStore() *MemoryJobStore { return &MemoryJobStore{m: map[JobID]Job{}} }

func (s *MemoryJobStore) Save(job Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[job.ID] = job
	return nil
}

func (s *MemoryJobStore) Get(id JobID) (Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.m[id]
	if !ok {
		return Job{}, fmt.Errorf("queue: job not found: %s", id)
	}
	return j, nil
}

func (s *MemoryJobStore) ListByState(state State) ([]Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Job
	for _, j := range s.m {
		if j.State == state {
			out = append(out, j)
		}
	}
	sortJobs(out)
	return out, nil
}

func (s *MemoryJobStore) ListByBatch(batch BatchID) ([]Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Job
	for _, j := range s.m {
		if j.BatchID == batch {
			out = append(out, j)
		}
	}
	sortJobs(out)
	return out, nil
}

func sortJobs(js []Job) {
	sort.Slice(js, func(i, j int) bool { return js[i].CreatedAt.Before(js[j].CreatedAt) })
}

// --- ChannelReporter adapter ---

type ChannelReporter struct {
	mu   sync.Mutex
	subs map[BatchID][]chan ProgressEvent
}

func NewChannelReporter() *ChannelReporter {
	return &ChannelReporter{subs: map[BatchID][]chan ProgressEvent{}}
}

func (r *ChannelReporter) Emit(ev ProgressEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ch := range r.subs[ev.BatchID] {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (r *ChannelReporter) Subscribe(batch BatchID) (<-chan ProgressEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ch := make(chan ProgressEvent, 256)
	r.subs[batch] = append(r.subs[batch], ch)
	return ch, nil
}

// --- RenderDispatcher adapter ---

// RenderDispatcher adapts a render.Renderer + recipe.Store to the
// WorkerDispatcher port, resolving a Job's RecipeID to a Recipe before render.
type RenderDispatcher struct {
	renderer interface {
		Render(rec recipe.Recipe, raw download.RawVideo) (RenderResult, error)
	}
	recipes recipe.Store
	raw     download.RawVideo
}

func NewRenderDispatcher(
	renderer interface {
		Render(rec recipe.Recipe, raw download.RawVideo) (RenderResult, error)
	},
	recipes recipe.Store,
	raw download.RawVideo,
) *RenderDispatcher {
	return &RenderDispatcher{renderer: renderer, recipes: recipes, raw: raw}
}

func (d *RenderDispatcher) Dispatch(job Job) (RenderResult, error) {
	rec, err := d.recipes.Get(job.RecipeID)
	if err != nil {
		return RenderResult{}, fmt.Errorf("%w: recipe lookup: %v", download.ErrPermanent, err)
	}
	return d.renderer.Render(rec, d.raw)
}
