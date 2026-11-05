package cli

import (
	"fmt"
	"io"
	"sort"

	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/manifest"
	"github.com/open-southeners/lambdary/internal/queue"
)

// declareQueues creates the queues lambdary.yml's queues block and the
// functions' sqs events name. Settings written in either are applied; a queue
// declared twice with different settings keeps the first and warns. A queue
// declared without settings is only created, so attributes a client set
// through the API survive a reload.
func declareQueues(store *queue.Store, cfg *manifest.Config, fns []discovery.Function, errOut io.Writer) {
	type declaration struct {
		settings manifest.QueueSettings
		source   string
	}

	declared := map[string]declaration{}

	var order []string

	declare := func(name string, settings manifest.QueueSettings, source string) {
		prev, ok := declared[name]

		switch {
		case !ok:
			order = append(order, name)
		case settings == (manifest.QueueSettings{}) || settings == prev.settings:
			return
		case prev.settings != (manifest.QueueSettings{}):
			fmt.Fprintf(errOut, "warning: queue %s: %s sets different settings than %s; keeping %s's\n", name, source, prev.source, prev.source)
			return
		}

		declared[name] = declaration{settings: settings, source: source}
	}

	if cfg != nil {
		names := make([]string, 0, len(cfg.Queues))
		for name := range cfg.Queues {
			names = append(names, name)
		}
		sort.Strings(names)

		for _, name := range names {
			declare(name, cfg.Queues[name], "lambdary.yml")
		}
	}

	for _, fn := range fns {
		if fn.Manifest == nil || fn.Version != "" {
			continue
		}

		for _, ev := range fn.Manifest.Events {
			if ev.SQS != nil {
				declare(ev.SQS.Queue, ev.SQS.QueueSettings, "function "+fn.Name)
			}
		}
	}

	for _, name := range order {
		d := declared[name]

		var err error
		if d.settings == (manifest.QueueSettings{}) {
			err = store.Create(name, queue.Settings{})
		} else {
			err = store.Ensure(name, d.settings.Settings())
		}

		if err != nil {
			fmt.Fprintf(errOut, "warning: queue %s: %s\n", name, err)
		}
	}
}

// renderQueues tells the user where the local SQS endpoint is, when there
// are queues.
func renderQueues(out io.Writer, port int, names []string) {
	if len(names) == 0 {
		return
	}

	fmt.Fprintf(out, "\nSQS queues: %v\n", names)
	fmt.Fprintf(out, "  set AWS_ENDPOINT_URL_SQS=http://127.0.0.1:%d for the AWS CLI and SDKs\n", port)
}

// disabledNote marks a disabled event source in the startup summary.
func disabledNote(enabled bool) string {
	if enabled {
		return ""
	}

	return " (disabled)"
}
