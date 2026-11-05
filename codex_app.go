package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const codexRemotePinnedSection = "Pinned"

type codexAppThread struct {
	ID         string           `json:"id"`
	CWD        string           `json:"cwd"`
	Originator string           `json:"originator"`
	ProjectID  *string          `json:"projectId"`
	Section    *codexAppSection `json:"section"`
	Status     struct {
		Type string `json:"type"`
	} `json:"status"`
}

type codexAppSection struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type codexAppProject struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type codexAppRPC struct {
	command *exec.Cmd
	cancel  context.CancelFunc
	input   io.WriteCloser
	output  *bufio.Scanner
	nextID  int
}

func openCodexAppRPC(config Config, task Task, timeout time.Duration) (*codexAppRPC, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	command := exec.CommandContext(ctx, config.LocalCodexBin, "app-server", "--listen", "stdio://")
	command.Dir = task.Worktree
	command.Env = localEnvironment(config)
	input, err := command.StdinPipe()
	if err != nil {
		cancel()
		return nil, errors.New("cannot open Codex app control input")
	}
	output, err := command.StdoutPipe()
	if err != nil {
		cancel()
		return nil, errors.New("cannot open Codex app control output")
	}
	command.Stderr = &boundedOutput{limit: 64 << 10}
	if err = command.Start(); err != nil {
		cancel()
		return nil, errors.New("cannot start Codex app control")
	}
	client := &codexAppRPC{command: command, cancel: cancel, input: input, output: bufio.NewScanner(output)}
	client.output.Buffer(make([]byte, 64<<10), 8<<20)
	initialize := map[string]any{
		"capabilities": map[string]any{"experimentalApi": true},
		"clientInfo":   map[string]string{"name": "codex-remote-runner", "title": "Codex Remote Runner", "version": Version},
	}
	if err = client.call("initialize", initialize, nil); err != nil {
		client.close()
		return nil, err
	}
	if err = client.notify("initialized", map[string]any{}); err != nil {
		client.close()
		return nil, err
	}
	return client, nil
}

func (c *codexAppRPC) close() {
	_ = c.input.Close()
	c.cancel()
	_ = c.command.Wait()
}

func (c *codexAppRPC) notify(method string, params any) error {
	return c.write(map[string]any{"method": method, "params": params})
}

func (c *codexAppRPC) call(method string, params any, result any) error {
	c.nextID++
	id := c.nextID
	if err := c.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return err
	}
	for c.output.Scan() {
		response := struct {
			ID     *int            `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}{}
		if json.Unmarshal(c.output.Bytes(), &response) != nil || response.ID == nil || *response.ID != id {
			continue
		}
		if len(response.Error) != 0 && string(response.Error) != "null" {
			return decodeCodexAppError(response.Error)
		}
		if result == nil || len(response.Result) == 0 {
			return nil
		}
		if json.Unmarshal(response.Result, result) != nil {
			return errors.New("unexpected Codex app response")
		}
		return nil
	}
	return errors.New("Codex app control ended before responding")
}

func (c *codexAppRPC) write(value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if _, err = c.input.Write(raw); err != nil {
		return errors.New("cannot write Codex app request")
	}
	return nil
}

func withCodexApp(config Config, task Task, operation func(*codexAppRPC) (any, error)) (any, error) {
	client, err := openCodexAppRPC(config, task, 30*time.Second)
	if err != nil {
		return nil, err
	}
	defer client.close()
	return operation(client)
}

func findCodexAppTaskThread(client *codexAppRPC, task Task) (codexAppThread, bool, error) {
	if task.Conversation == "" && task.AppThread == "" {
		return codexAppThread{}, false, errors.New("task has no Codex App conversation")
	}
	matches := []struct {
		thread   codexAppThread
		archived bool
	}{}
	var explicit codexAppThread
	if task.AppThread != "" {
		response := struct {
			Thread codexAppThread `json:"thread"`
		}{}
		if err := client.call("thread/read", map[string]any{"threadId": task.AppThread, "includeTurns": false}, &response); err != nil {
			return codexAppThread{}, false, err
		}
		if response.Thread.ID != task.AppThread || !strings.EqualFold(filepath.Clean(response.Thread.CWD), filepath.Clean(task.Worktree)) {
			return codexAppThread{}, false, errors.New("Codex App thread does not belong to this task worktree")
		}
		explicit = response.Thread
	}
	for _, archived := range []bool{false, true} {
		response := struct {
			Data []codexAppThread `json:"data"`
		}{}
		params := map[string]any{"archived": archived, "cwd": filepath.Clean(task.Worktree), "limit": 100, "useStateDbOnly": false}
		if err := client.call("thread/list", params, &response); err != nil {
			return codexAppThread{}, false, err
		}
		for _, thread := range response.Data {
			identityMatches := task.AppThread != "" && thread.ID == task.AppThread
			if task.AppThread == "" {
				identityMatches = thread.Originator == "T3 Code"
			}
			if identityMatches && strings.EqualFold(filepath.Clean(thread.CWD), filepath.Clean(task.Worktree)) {
				matches = append(matches, struct {
					thread   codexAppThread
					archived bool
				}{thread: thread, archived: archived})
			}
		}
	}
	if len(matches) == 0 && explicit.ID != "" {
		return explicit, false, nil
	}
	if len(matches) == 0 {
		return codexAppThread{}, false, errors.New("no Codex App thread matches this task")
	}
	if len(matches) != 1 {
		return codexAppThread{}, false, errors.New("multiple Codex App threads match this task; refuse ambiguous change")
	}
	return matches[0].thread, matches[0].archived, nil
}

func adoptCodexAppThread(config Config, task Task, threadID string) (any, error) {
	if strings.TrimSpace(threadID) == "" {
		return nil, errors.New("provide a Codex App thread ID")
	}
	return withCodexApp(config, task, func(client *codexAppRPC) (any, error) {
		response := struct {
			Thread codexAppThread `json:"thread"`
		}{}
		if err := client.call("thread/read", map[string]any{"threadId": threadID, "includeTurns": false}, &response); err != nil {
			return nil, err
		}
		if response.Thread.ID != threadID || !strings.EqualFold(filepath.Clean(response.Thread.CWD), filepath.Clean(task.Worktree)) {
			return nil, errors.New("Codex App thread does not belong to this task worktree")
		}
		if response.Thread.Status.Type != "idle" && response.Thread.Status.Type != "notLoaded" {
			return nil, errors.New("Codex App thread is not idle")
		}
		if err := client.call("thread/resume", map[string]any{"threadId": threadID, "cwd": task.Worktree}, nil); err != nil {
			return nil, errors.New("Codex App must release its active writer before this thread can be adopted")
		}
		if err := client.call("thread/unsubscribe", map[string]string{"threadId": threadID}, nil); err != nil {
			return nil, errors.New("cannot release the adoption verification writer")
		}
		return map[string]string{"status": "adopted", "thread_id": threadID}, nil
	})
}

func createCodexAppThread(config Config, task Task, title, projectID string) (string, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		title = task.Ticket
	}
	if len(title) > 200 || strings.ContainsAny(title, "\r\n") {
		return "", errors.New("Codex App title must contain at most 200 characters on one line")
	}
	value, err := withCodexApp(config, task, func(client *codexAppRPC) (any, error) {
		params := map[string]any{"cwd": task.Worktree, "projectId": nil}
		if projectID != "" {
			project := struct {
				Project codexAppProject `json:"project"`
			}{}
			if err := client.call("project/read", map[string]string{"projectId": projectID}, &project); err != nil || project.Project.ID != projectID {
				return nil, errors.New("Codex App project is unavailable")
			}
			params["projectId"] = projectID
		}
		response := struct {
			Thread codexAppThread `json:"thread"`
		}{}
		if err := client.call("thread/start", params, &response); err != nil {
			return nil, err
		}
		if response.Thread.ID == "" || !strings.EqualFold(filepath.Clean(response.Thread.CWD), filepath.Clean(task.Worktree)) {
			return nil, errors.New("Codex App created an unexpected thread")
		}
		if err := client.call("thread/name/set", map[string]string{"threadId": response.Thread.ID, "name": title}, nil); err != nil {
			return nil, err
		}
		if err := client.call("thread/unsubscribe", map[string]string{"threadId": response.Thread.ID}, nil); err != nil {
			return nil, errors.New("cannot release the new Codex App thread writer")
		}
		return response.Thread.ID, nil
	})
	if err != nil {
		return "", err
	}
	threadID, ok := value.(string)
	if !ok || threadID == "" {
		return "", errors.New("Codex App thread creation returned an invalid ID")
	}
	return threadID, nil
}

func runCodexAppTurn(config Config, task Task, prompt string, fullAccess bool) (StartResult, error) {
	result := StartResult{}
	client, err := openCodexAppRPC(config, task, 4*time.Hour)
	if err != nil {
		return result, err
	}
	defer client.close()
	thread, archived, err := findCodexAppTaskThread(client, task)
	if err != nil {
		return result, err
	}
	if archived {
		return result, errors.New("unarchive the Codex App thread before running it")
	}
	if thread.Status.Type != "idle" && thread.Status.Type != "notLoaded" {
		return result, errors.New("Codex App thread already has active work")
	}
	if err = client.call("thread/resume", map[string]any{"threadId": thread.ID, "cwd": task.Worktree}, nil); err != nil {
		return result, errors.New("Codex App thread writer is unavailable; release it in the current client before retrying")
	}
	params := map[string]any{
		"threadId": thread.ID,
		"cwd":      task.Worktree,
		"input":    []map[string]string{{"type": "text", "text": prompt}},
	}
	if fullAccess {
		params["approvalPolicy"] = "never"
		params["sandboxPolicy"] = map[string]string{"type": "dangerFullAccess"}
	}
	return client.startTurnAndWait(thread.ID, params)
}

func (c *codexAppRPC) startTurnAndWait(threadID string, params map[string]any) (StartResult, error) {
	c.nextID++
	requestID := c.nextID
	if err := c.write(map[string]any{"id": requestID, "method": "turn/start", "params": params}); err != nil {
		return StartResult{}, err
	}
	turnID := ""
	for c.output.Scan() {
		message := struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
			Params json.RawMessage `json:"params"`
		}{}
		if json.Unmarshal(c.output.Bytes(), &message) != nil {
			continue
		}
		if message.ID != nil {
			if message.Method != "" {
				return StartResult{}, errors.New("Codex App turn requires an interactive approval; use the App or an explicitly authorized full-access turn")
			}
			if *message.ID != requestID {
				continue
			}
			if len(message.Error) != 0 && string(message.Error) != "null" {
				return StartResult{}, decodeCodexAppError(message.Error)
			}
			response := struct {
				Turn struct {
					ID string `json:"id"`
				} `json:"turn"`
			}{}
			if json.Unmarshal(message.Result, &response) != nil || response.Turn.ID == "" {
				return StartResult{}, errors.New("Codex App turn response was incomplete")
			}
			turnID = response.Turn.ID
			continue
		}
		if message.Method != "turn/completed" || turnID == "" {
			continue
		}
		completed := struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"turn"`
		}{}
		if json.Unmarshal(message.Params, &completed) != nil || completed.ThreadID != threadID || completed.Turn.ID != turnID {
			continue
		}
		if completed.Turn.Status != "completed" {
			return StartResult{}, errors.New("Codex App turn did not complete successfully")
		}
		return StartResult{ID: threadID, Job: turnID}, nil
	}
	return StartResult{}, errors.New("Codex App control ended before the turn completed")
}

func decodeCodexAppError(raw json.RawMessage) error {
	value := struct {
		Message string `json:"message"`
	}{}
	if json.Unmarshal(raw, &value) != nil || strings.TrimSpace(value.Message) == "" {
		return errors.New("Codex App request was rejected")
	}
	message := strings.TrimSpace(value.Message)
	if len(message) > 500 {
		message = message[:500]
	}
	return errors.New("Codex App: " + message)
}

func manageCodexAppArchive(config Config, task Task, archived bool) (any, error) {
	return withCodexApp(config, task, func(client *codexAppRPC) (any, error) {
		thread, current, err := findCodexAppTaskThread(client, task)
		if err != nil {
			return nil, err
		}
		if current != archived {
			method := "thread/archive"
			if !archived {
				method = "thread/unarchive"
			}
			if err = client.call(method, map[string]string{"threadId": thread.ID}, nil); err != nil {
				return nil, err
			}
		}
		status := "active"
		if archived {
			status = "archived"
		}
		return map[string]string{"status": status, "thread_id": thread.ID}, nil
	})
}

func manageCodexAppPin(config Config, task Task, pinned bool) (any, error) {
	return withCodexApp(config, task, func(client *codexAppRPC) (any, error) {
		thread, archived, err := findCodexAppTaskThread(client, task)
		if err != nil {
			return nil, err
		}
		if archived {
			return nil, errors.New("unarchive the task before changing its pinned section")
		}
		sections := struct {
			Data []codexAppSection `json:"data"`
		}{}
		if err = client.call("threadSection/list", map[string]any{"limit": 100}, &sections); err != nil {
			return nil, err
		}
		var pinnedSections []codexAppSection
		for _, section := range sections.Data {
			if section.Name == codexRemotePinnedSection {
				pinnedSections = append(pinnedSections, section)
			}
		}
		if len(pinnedSections) > 1 {
			return nil, errors.New("multiple Pinned sections exist; refuse ambiguous change")
		}
		var section codexAppSection
		if len(pinnedSections) == 1 {
			section = pinnedSections[0]
		}
		if pinned && section.ID == "" {
			created := struct {
				Section codexAppSection `json:"section"`
			}{}
			params := map[string]any{"name": codexRemotePinnedSection, "appearance": map[string]string{"icon": "pin"}}
			if err = client.call("threadSection/create", params, &created); err != nil {
				return nil, err
			}
			section = created.Section
		}
		destination := any(nil)
		status := "unpinned"
		if pinned {
			destination = section.ID
			status = "pinned"
		} else if thread.Section == nil || thread.Section.ID != section.ID {
			return map[string]string{"status": status, "thread_id": thread.ID}, nil
		}
		if err = client.call("thread/section/move", map[string]any{"threadId": thread.ID, "sectionId": destination}, nil); err != nil {
			return nil, err
		}
		return map[string]string{"status": status, "thread_id": thread.ID}, nil
	})
}

func manageCodexAppProject(config Config, task Task, projectID string) (any, error) {
	return withCodexApp(config, task, func(client *codexAppRPC) (any, error) {
		thread, archived, err := findCodexAppTaskThread(client, task)
		if err != nil {
			return nil, err
		}
		if archived {
			return nil, errors.New("unarchive the task before changing its project")
		}
		projectName := ""
		if projectID != "" {
			project := struct {
				Project codexAppProject `json:"project"`
			}{}
			if err = client.call("project/read", map[string]string{"projectId": projectID}, &project); err != nil {
				return nil, err
			}
			if project.Project.ID != projectID {
				return nil, errors.New("Codex app returned another project")
			}
			projectName = project.Project.Name
		}
		if err = client.call("thread/metadata/update", map[string]any{"threadId": thread.ID, "projectId": projectID}, nil); err != nil {
			return nil, err
		}
		return map[string]string{"status": "project-updated", "thread_id": thread.ID, "project_id": projectID, "project_name": projectName}, nil
	})
}

func manageCodexAppName(config Config, task Task, title string) (any, error) {
	title = strings.TrimSpace(title)
	if title == "" || len(title) > 200 || strings.ContainsAny(title, "\r\n") {
		return nil, errors.New("Codex App title must contain 1 to 200 characters on one line")
	}
	return withCodexApp(config, task, func(client *codexAppRPC) (any, error) {
		thread, archived, err := findCodexAppTaskThread(client, task)
		if err != nil {
			return nil, err
		}
		if archived {
			return nil, errors.New("unarchive the task before renaming it")
		}
		if err = client.call("thread/name/set", map[string]string{"threadId": thread.ID, "name": title}, nil); err != nil {
			return nil, err
		}
		return map[string]string{"status": "renamed", "thread_id": thread.ID, "title": title}, nil
	})
}

func listCodexAppProjects(config Config, task Task) (any, error) {
	return withCodexApp(config, task, func(client *codexAppRPC) (any, error) {
		response := struct {
			Data []codexAppProject `json:"data"`
		}{}
		if err := client.call("project/list", map[string]any{"limit": 100, "sortKey": "position", "sortDirection": "asc"}, &response); err != nil {
			return nil, err
		}
		return map[string]any{"projects": response.Data}, nil
	})
}
