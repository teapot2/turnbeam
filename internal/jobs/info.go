package jobs

// Info exposes only the provenance/readiness needed by the pipeline, not paths.
// TranscriptionReady includes a fully saved job interrupted before Finish.
type Info struct {
	InputSHA256        string
	TranscriptionReady bool
	// Chunks is the planned chunk count (0 before planning); ChunksDone are saved.
	Chunks, ChunksDone int
}

// Info returns a locked snapshot without treating output publication as complete.
func (j *Job) Info() (Info, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.usable(); err != nil {
		return Info{}, err
	}
	info := Info{InputSHA256: j.m.InputSHA256, ChunksDone: len(j.m.Completed)}
	if j.m.Plan != nil {
		info.Chunks = len(*j.m.Plan)
		info.TranscriptionReady = info.ChunksDone == info.Chunks
	}
	return info, nil
}
