package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

type webInspection struct {
	URL           string   `json:"url"`
	Screenshot    []byte   `json:"screenshot,omitempty"`
	Console       []string `json:"console"`
	Exceptions    []string `json:"exceptions"`
	NetworkErrors []string `json:"network_errors"`
}

func windowsWebLogs(task Task, tail int) (map[string]any, error) {
	if tail == 0 {
		tail = 200
	}
	if tail < 1 || tail > 1000 {
		return nil, errors.New("web log tail must be between 1 and 1000 lines")
	}
	raw, err := os.ReadFile(windowsTaskControlPath(task, "web.log"))
	if err != nil {
		return nil, errors.New("workspace web service has no readable log")
	}
	if len(raw) > 1<<20 {
		raw = raw[len(raw)-(1<<20):]
	}
	lines := strings.Split(string(raw), "\n")
	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	return map[string]any{"lines": lines, "truncated": len(raw) == 1<<20}, nil
}

func inspectWindowsWeb(policy windowsWorkerPolicy, task Task, requestPath string) (webInspection, error) {
	if policy.BrowserBin == "" {
		return webInspection{}, errors.New("browser inspection is not configured on the Windows worker")
	}
	status, err := windowsWebStatus(task, requestPath)
	if err != nil {
		return webInspection{}, err
	}
	if status["status"] != "online" {
		return webInspection{}, errors.New("workspace web service is not online")
	}
	target, _ := status["journey_url"].(string)
	if target == "" {
		return webInspection{}, errors.New("workspace web service has no verified journey URL")
	}
	allocatorOptions := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(policy.BrowserBin),
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-first-run", true),
	)
	allocator, cancelAllocator := chromedp.NewExecAllocator(context.Background(), allocatorOptions...)
	defer cancelAllocator()
	ctx, cancel := chromedp.NewContext(allocator)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, 75*time.Second)
	defer cancelTimeout()
	result := webInspection{URL: target, Console: []string{}, Exceptions: []string{}, NetworkErrors: []string{}}
	var lock sync.Mutex
	appendBounded := func(target *[]string, value string) {
		lock.Lock()
		defer lock.Unlock()
		if len(*target) >= 100 {
			return
		}
		value = strings.TrimSpace(value)
		if len(value) > 1000 {
			value = value[:1000] + "…"
		}
		*target = append(*target, value)
	}
	chromedp.ListenTarget(ctx, func(event any) {
		switch value := event.(type) {
		case *cdpruntime.EventConsoleAPICalled:
			parts := make([]string, 0, len(value.Args))
			for _, arg := range value.Args {
				text := arg.Description
				if text == "" && len(arg.Value) != 0 {
					text = string(arg.Value)
				}
				parts = append(parts, text)
			}
			appendBounded(&result.Console, fmt.Sprintf("%s: %s", value.Type, strings.Join(parts, " ")))
		case *cdpruntime.EventExceptionThrown:
			appendBounded(&result.Exceptions, value.ExceptionDetails.Text)
		case *network.EventLoadingFailed:
			appendBounded(&result.NetworkErrors, value.ErrorText)
		case *network.EventResponseReceived:
			if value.Response.Status >= 400 {
				appendBounded(&result.NetworkErrors, fmt.Sprintf("HTTP %d %s", value.Response.Status, value.Response.URL))
			}
		}
	})
	err = chromedp.Run(ctx,
		network.Enable(),
		chromedp.Navigate(target),
		chromedp.WaitReady("body", chromedp.ByQuery),
		chromedp.Sleep(2*time.Second),
		chromedp.FullScreenshot(&result.Screenshot, 90),
	)
	if err != nil {
		return webInspection{}, errors.New("browser inspection failed; inspect the host privately")
	}
	return result, nil
}
