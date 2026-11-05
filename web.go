package main

import (
	"errors"
	"os"
	"path/filepath"
)

func (r *Runner) PreparePreview(ticket string) (Task, error) {
	task, err := r.State.Task(ticket)
	if err != nil {
		return task, err
	}
	if task.Host != "windows" || task.Phase != "active" {
		return task, errors.New("preview preparation requires an active Windows worktree task")
	}
	if err = r.requireWindowsWorkerReady(task); err != nil {
		return task, err
	}
	if err = r.localControl().PreparePreview(task); err != nil {
		return task, err
	}
	return task, nil
}

func (r *Runner) WebStatus(ticket, requestPath string) (map[string]any, error) {
	task, err := r.State.Task(ticket)
	if err != nil {
		return nil, err
	}
	if task.Host != "windows" || task.Phase != "active" {
		return nil, errors.New("web status requires an active Windows worktree task")
	}
	return r.localControl().WebStatus(task, requestPath)
}

func (r *Runner) StopWeb(ticket string) error {
	task, err := r.State.Task(ticket)
	if err != nil {
		return err
	}
	if task.Host != "windows" {
		return errors.New("stop-web requires a Windows worktree task")
	}
	return r.localControl().StopWeb(task)
}

func (r *Runner) WebLogs(ticket string, tail int) (map[string]any, error) {
	task, err := r.State.Task(ticket)
	if err != nil {
		return nil, err
	}
	if task.Host != "windows" {
		return nil, errors.New("web-logs requires a Windows worktree task")
	}
	return r.localControl().WebLogs(task, tail)
}

func (r *Runner) WebInspect(ticket, requestPath, outputFile string) (map[string]any, error) {
	task, err := r.State.Task(ticket)
	if err != nil {
		return nil, err
	}
	if task.Host != "windows" || task.Phase != "active" {
		return nil, errors.New("web-inspect requires an active Windows worktree task")
	}
	if outputFile == "" {
		return nil, errors.New("web-inspect requires --output-file")
	}
	inspection, err := r.localControl().WebInspect(task, requestPath)
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.Abs(expandHome(outputFile))
	if err != nil {
		return nil, errors.New("cannot resolve screenshot output path")
	}
	file, err := os.OpenFile(resolved, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, errors.New("screenshot output file already exists or cannot be created")
	}
	if _, err = file.Write(inspection.Screenshot); err != nil {
		file.Close()
		_ = os.Remove(resolved)
		return nil, errors.New("cannot write screenshot output file")
	}
	if err = file.Close(); err != nil {
		_ = os.Remove(resolved)
		return nil, errors.New("cannot finish screenshot output file")
	}
	return map[string]any{"url": inspection.URL, "screenshot_file": resolved, "console": inspection.Console, "exceptions": inspection.Exceptions, "network_errors": inspection.NetworkErrors}, nil
}
