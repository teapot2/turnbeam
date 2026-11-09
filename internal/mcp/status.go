package mcp

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/teapot2/turnbeam/internal/apple"
	"github.com/teapot2/turnbeam/internal/credentials"
	"github.com/teapot2/turnbeam/internal/pipeline"
	"github.com/teapot2/turnbeam/internal/setup"
)

type checkView struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	Fix     string `json:"fix,omitempty"`
}

type modelView struct {
	Name      string `json:"name"`
	Runs      string `json:"runs"`
	Ready     bool   `json:"ready"`
	Status    string `json:"status"`
	SizeMB    int64  `json:"size_mb,omitempty"`
	Languages string `json:"languages,omitempty"`
	License   string `json:"license,omitempty"`
	Default   bool   `json:"default,omitempty"`
}

type statusView struct {
	Ready        bool        `json:"ready"`
	Checks       []checkView `json:"checks"`
	Models       []modelView `json:"models"`
	DefaultModel string      `json:"default_model"`
	Roots        []string    `json:"roots"`
	Jobs         []jobView   `json:"jobs"`
}

// status mirrors `turnbeam doctor` and `turnbeam models` as data. A missing
// Meta key is not a failure when a local model can transcribe instead.
func status(ctx context.Context, s *Server, _ *call, raw json.RawMessage) (*toolResult, error) {
	if err := decodeArgs(raw, &struct{}{}); err != nil {
		return nil, err
	}
	checks, err := s.Installation.Doctor(ctx)
	if err != nil && !errors.Is(err, setup.ErrDoctorFailed) {
		return nil, err
	}
	statuses, err := s.Installation.LocalStatus(ctx)
	if err != nil {
		return nil, err
	}
	source, available, keyErr := s.KeyStatus(ctx)
	local := false
	for _, check := range checks {
		local = local || check.OK && check.LocalModel != ""
	}
	checks = append(checks, keyCheck(source, available, keyErr, local))
	// An unusable default is reported as none; transcribe explains it.
	defaultModel, _ := s.defaultModel(ctx)
	view := statusView{Ready: true, Checks: []checkView{}, DefaultModel: defaultModel, Roots: s.roots, Jobs: s.jobs.list()}
	for _, check := range checks {
		view.Ready = view.Ready && check.OK
		view.Checks = append(view.Checks, checkView{Name: check.Name, OK: check.OK, Message: check.Message, Fix: check.Fix})
	}
	meta := modelView{Name: pipeline.MetaModel, Runs: "Meta API", Ready: available && keyErr == nil, Default: defaultModel == pipeline.MetaModel}
	switch {
	case keyErr != nil:
		meta.Status = "key could not be read"
	case !available:
		meta.Status = "needs a key; run `turnbeam key set`"
	default:
		meta.Status = "ready"
	}
	view.Models = append(view.Models, meta)
	for _, status := range statuses {
		model := modelView{Name: status.Model.Name, Runs: "on device", Ready: status.Installed && status.Err == nil, Status: "installed",
			SizeMB: (status.Model.Size() + 500_000) / 1_000_000, Languages: status.Model.Languages, License: status.Model.License, Default: defaultModel == status.Model.Name}
		switch {
		case status.Err != nil:
			model.Status = "damaged; run `turnbeam setup --model " + status.Model.Name + "`"
		case !status.Installed:
			model.Status = "not installed; run `turnbeam setup --model " + status.Model.Name + "`"
		}
		view.Models = append(view.Models, model)
	}
	view.Models = append(view.Models, s.appleView(ctx, defaultModel))
	return result(view)
}

// appleView reports Apple's transcriber for the Mac's language.
func (s *Server) appleView(ctx context.Context, defaultModel string) modelView {
	view := modelView{Name: apple.Model, Runs: "on device", Languages: "many; pass language", License: "Apple (macOS)", Default: defaultModel == apple.Model,
		Status: "not set up; run `turnbeam setup --model apple`"}
	if !s.Apple.Built() {
		return view
	}
	state, err := s.Apple.Status(ctx, "")
	switch {
	case errors.Is(err, apple.ErrUnavailable):
		view.Status = "needs macOS 26 or later"
	case err != nil:
		view.Status = "could not be checked"
	case state.Status == "installed":
		view.Ready, view.Status = true, "installed ("+state.Locale+")"
	default:
		view.Status = state.Locale + " not downloaded; run `turnbeam setup --model apple`"
	}
	return view
}

func keyCheck(source credentials.Source, available bool, err error, localModel bool) setup.Check {
	check := setup.Check{Name: "Meta API key"}
	switch {
	case err != nil:
		check.Message, check.Fix = "could not be read", "unlock your login keychain, or set META_API_KEY"
	case !available && localModel:
		check.Message, check.OK = "not configured (only model meta needs one)", true
	case !available:
		check.Message, check.Fix = "not configured", "run `turnbeam key set`, or set META_API_KEY, or install a local model with `turnbeam setup --model parakeet`"
	case source == credentials.Environment:
		check.Message, check.OK = "from the META_API_KEY environment variable", true
	default:
		check.Message, check.OK = "stored in Keychain", true
	}
	return check
}
