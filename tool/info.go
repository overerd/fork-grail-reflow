// Copyright 2017 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package tool

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/grailbio/base/data"
	"github.com/grailbio/base/digest"
	"github.com/grailbio/base/state"
	"github.com/grailbio/reflow"
	"github.com/grailbio/reflow/assoc"
	"github.com/grailbio/reflow/errors"
	"github.com/grailbio/reflow/log"
	"github.com/grailbio/reflow/pool"
	"github.com/grailbio/reflow/repository"
	"github.com/grailbio/reflow/runner"
	"github.com/grailbio/reflow/taskdb"
)

func (c *Cmd) info(ctx context.Context, args ...string) {
	flags := flag.NewFlagSet("info", flag.ExitOnError)
	exactCostFlag := flags.Bool("exact_cost", false, "show exact cost for runs (if available)")
	fullFlag := flags.Bool("full", false, "shows full ids for runs/tasks and full error messages")

	help := `Info displays general information about Reflow objects.

Info displays information about:

	- runs
	- tasks (execs)
	- cached filesets
	- files
	- allocs

Where an opaque identifier is given (a sha256 checksum), info looks
it up in all candidate data sources and displays the first match.
Abbreviated IDs are expanded where possible.

If given a RunId or TaskId, the following info is shown:
` + runTaskHelp + `
` + costHelp + `

Exact costs are shown (if available) only for runs.
`
	c.Parse(flags, args, help, "info [-exact_cost] names...")
	if flags.NArg() == 0 {
		flags.Usage()
	}
	var tdb taskdb.TaskDB
	err := c.Config.Instance(&tdb)
	if err != nil {
		log.Debug("taskdb: ", err)
	}
	for _, arg := range flags.Args() {
		n, err := parseName(arg)
		if err != nil {
			c.Fatalf("parse name %s: %v", arg, err)
		}
		var tw tabwriter.Writer
		tw.Init(c.Stdout, 4, 4, 1, ' ', 0)
		switch n.Kind {
		case idName:
			// Always print local run info if available.
			if ok := c.printLocalRunInfo(&tw, n.ID); ok {
				fmt.Fprintln(&tw, divider)
			}
			switch {
			case c.printTdbRunInfo(ctx, &tw, n.ID, *exactCostFlag, *fullFlag):
			case c.printTdbTaskInfo(ctx, &tw, n.ID):
			case c.printCacheInfo(ctx, &tw, n.ID):
			case c.printFileInfo(ctx, &tw, n.ID):
			default:
				c.Fatalf("unable to resolve id %s", arg)
			}
		case execName:
			var inspect reflow.ExecInspect
			if tdb != nil {
				inspect, err = c.liveExecInspect(ctx, n)
				if err != nil {
					c.Fatalf("error inspecting exec %q: %s", arg, err)
				}
			} else {
				alloc, aerr := c.CurrentPool(ctx).Alloc(ctx, n.AllocID)
				if aerr != nil {
					c.Fatal(aerr)
				}
				exec, eerr := alloc.Get(ctx, n.ID)
				if eerr != nil {
					c.Fatalf("failed to fetch exec for %q: %s", arg, eerr)
				}
				resp, rerr := exec.Inspect(ctx, nil)
				if rerr != nil {
					c.Fatalf("error inspecting exec %q: %s", arg, rerr)
				}
				inspect = *resp.Inspect
			}
			var result reflow.Result
			if inspect.State == "complete" {
				result, err = c.liveExecResult(ctx, n)
				if err != nil {
					c.Errorf("failed to fetch result for exec %s: %s\n", arg, err)
				}
			}
			fmt.Fprintln(&tw, arg, "(exec)")
			c.printExec(ctx, &tw, inspect, result)
		case allocName:
			var (
				execs   []reflow.Exec
				inspect pool.AllocInspect
			)
			if tdb != nil {
				inspect, err = c.allocInspect(ctx, n)
				c.must(err)
				execs, err = c.allocExecs(ctx, n)
				c.must(err)
			} else {
				alloc, err := c.CurrentPool(ctx).Alloc(ctx, n.AllocID)
				c.must(err)
				inspect, err = alloc.Inspect(ctx)
				c.must(err)
				execs, err = alloc.Execs(ctx)
				c.must(err)
			}
			execs, err := c.allocExecs(ctx, n)
			c.must(err)
			fmt.Fprintln(&tw, arg, "(alloc)")
			c.printAlloc(ctx, &tw, inspect, execs)
		}
		tw.Flush()
	}
}

// printLocalRunInfo prints run info from the local run dir for the given run id (if found)
func (c *Cmd) printLocalRunInfo(w io.Writer, id digest.Digest) bool {
	f, err := os.Open(c.rundir())
	if os.IsNotExist(err) {
		return false
	} else if err != nil {
		c.Errorln(err)
		return false
	}
	infos, err := f.Readdir(-1)
	if err != nil {
		c.Errorln(err)
		return false
	}
	if id.IsAbbrev() {
		for _, info := range infos {
			d := info.Name()
			if filepath.Ext(d) != ".json" {
				continue
			}
			d = d[:len(d)-5]
			fullID, err := reflow.Digester.Parse(d)
			if err != nil {
				c.Errorf("%s: %v\n", info.Name(), err)
				continue
			}
			if fullID.Expands(id) {
				id = fullID
				break
			}
		}

	}
	base := filepath.Join(c.rundir(), id.Hex())
	_, err = os.Stat(base + ".json")
	if os.IsNotExist(err) {
		return false
	} else if err != nil {
		c.Errorf("%s: %v\n", id.Short(), err)
		return false
	}
	statefile, err := state.Open(base)
	if err != nil {
		c.Errorf("%s: %v\n", id.Short(), err)
		return false
	}

	var state runner.State
	c.must(statefile.Unmarshal(&state))
	fmt.Fprintln(w, id.Hex(), "(run)")
	fmt.Fprintf(w, "\ttime:\t%s\n", state.Created.Local().Format(time.ANSIC))
	fmt.Fprintf(w, "\tprogram:\t%s\n", state.Program)
	if len(state.Params) > 0 {
		fmt.Fprintf(w, "\tparams:\n")
		var keys []string
		for k := range state.Params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(w, "\t\t%s:\t%s\n", k, state.Params[k])
		}
	}
	if len(state.Args) > 0 {
		fmt.Fprintf(w, "\targs:\n")
		for _, v := range state.Args {
			fmt.Fprintf(w, "\t\t%s\n", v)
		}
	}
	fmt.Fprintf(w, "\tphase:\t%s\n", state.Phase)
	if state.Err != nil {
		fmt.Fprintf(w, "\terror:\t%s\n", state.Err)
	}
	if state.Result != "" {
		fmt.Fprintf(w, "\tresult:\t%s\n", state.Result)
	}
	if _, err := os.Stat(base + ".runlog"); err == nil {
		fmt.Fprintf(w, "\tlog:\t%s.runlog\n", base)
	}
	return true
}

func (c *Cmd) printTdbRunInfo(ctx context.Context, w io.Writer, runId digest.Digest, exactCost, full bool) bool {
	q := taskdb.RunQuery{ID: taskdb.RunID(runId)}
	infos, err := c.runInfo(ctx, q, false /* liveOnly */, exactCost)
	if err != nil {
		c.Log.Debugf("RunQuery %v: %v", q, err)
	}
	if len(infos) == 0 {
		return false
	}
	c.writeRuns(infos, w, true, full)
	return true
}

func (c *Cmd) printTdbTaskInfo(ctx context.Context, w io.Writer, taskId digest.Digest) bool {
	q := taskdb.TaskQuery{ID: taskdb.TaskID(taskId)}
	infos, err := c.taskInfo(ctx, q, false /* liveOnly */, true /* cost */, nil)
	if err != nil {
		c.Log.Debugf("TaskQuery %v: %v", q, err)
	}
	if len(infos) == 0 {
		return false
	}
	printTaskHeader(w, true)
	for _, t := range infos {
		c.writeTask(t, w, true, true)
	}
	return true
}

func (c *Cmd) printCacheInfo(ctx context.Context, w io.Writer, id digest.Digest) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var ass assoc.Assoc
	err := c.Config.Instance(&ass)
	if err != nil {
		c.Fatal(err)
	}
	id, fsid, err := ass.Get(ctx, assoc.FilesetV2, id)
	switch {
	case err == nil:
		var repo reflow.Repository
		c.must(c.Config.Instance(&repo))
		var fs reflow.Fileset
		switch uErr := repository.Unmarshal(ctx, repo, fsid, &fs, assoc.FilesetV2); {
		case uErr == nil:
		case errors.Is(errors.NotExist, uErr):
			return false
		default:
			c.Fatalf("repository.Unmarshal %v: %v", fsid, uErr)
		}
		fmt.Fprintln(w, id.Hex(), "(cached fileset)")
		if fs.N() == 0 {
			fmt.Fprintln(w, "	(empty)")
		} else {
			c.printFileset(w, "	", fs)
		}
		return true
	case errors.Is(errors.NotExist, err):
		return false
	default:
		c.Fatalf("assoc.Get %s: %v", id.Hex(), err)
		return false
	}
}

func (c *Cmd) printFileInfo(ctx context.Context, w io.Writer, id digest.Digest) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var repo reflow.Repository
	c.must(c.Config.Instance(&repo))
	info, err := repo.Stat(ctx, id)
	switch {
	case err == nil:
		fmt.Fprintln(w, info.ID.Hex(), "(file)")
		fmt.Fprintf(w, "\tsize:\t%d\n", info.Size)
		return true
	case errors.Is(errors.NotExist, err):
		return false
	default:
		c.Fatalf("stat %v: %v", id.Hex(), err)
		return false
	}
}

func (c *Cmd) printAlloc(ctx context.Context, w io.Writer, inspect pool.AllocInspect, execs []reflow.Exec) {
	fmt.Fprintf(w, "\tmem:\t%s\n", data.Size(inspect.Resources["mem"]))
	fmt.Fprintf(w, "\tcpu:\t%.1f\n", inspect.Resources["cpu"])
	fmt.Fprintf(w, "\tdisk:\t%s\n", data.Size(inspect.Resources["disk"]))
	fmt.Fprintf(w, "\towner:\t%s\n", inspect.Meta.Owner)
	fmt.Fprintf(w, "\tkeepalive:\t%s (%s ago)\n", inspect.LastKeepalive, round(time.Since(inspect.LastKeepalive)))
	if expires := time.Until(inspect.Expires); expires < time.Duration(0) {
		fmt.Fprintf(w, "\texpires:\t%s (%s ago)\n", inspect.Expires, round(-expires))
	} else {
		fmt.Fprintf(w, "\texpires:\t%s (in %s)\n", inspect.Expires, round(expires))
	}
	if len(inspect.Meta.Labels) > 0 {
		var keys []string
		for key := range inspect.Meta.Labels {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		fmt.Fprintf(w, "\tlabels:\n")
		for _, key := range keys {
			fmt.Fprintf(w, "\t  %s\t%s\n", key, inspect.Meta.Labels[key])
		}
	}
	if len(execs) > 0 {
		fmt.Fprintf(w, "\texecs:\n")
		for _, exec := range execs {
			fmt.Fprintf(w, "\t  %s\n", exec.URI())
		}
	}
}

func (c *Cmd) printExec(ctx context.Context, w io.Writer, inspect reflow.ExecInspect, result reflow.Result) {
	fmt.Fprintf(w, "\tstate:\t%s\n", inspect.State)
	fmt.Fprintf(w, "\ttype:\t%s\n", inspect.Config.Type)
	if inspect.Config.Ident != "" {
		fmt.Fprintf(w, "\tident:\t%s\n", inspect.Config.Ident)
	}
	if inspect.Config.URL != "" {
		fmt.Fprintf(w, "\turl:\t%s\n", inspect.Config.URL)
	}
	if inspect.Config.Image != "" {
		fmt.Fprintf(w, "\timage:\t%s\n", inspect.Config.Image)
	}
	if inspect.Config.Cmd != "" {
		// Find synonymous filesets so we can abbreviate our output.
		syns := make([]int, len(inspect.Config.Args))
		for i := range inspect.Config.Args {
			if inspect.Config.Args[i].Fileset == nil {
				continue
			}
			for j := i + 1; j < len(inspect.Config.Args); j++ {
				if inspect.Config.Args[j].Fileset == nil {
					continue
				}
				if inspect.Config.Args[i].Fileset.Equal(*inspect.Config.Args[j].Fileset) {
					syns[i] = j
					break
				}
			}
		}
		args := make([]interface{}, len(inspect.Config.Args))
		for i := range args {
			args[i] = fmt.Sprintf("{{arg[%d]}}", i)
		}
		fmt.Fprintf(w, "\tcmd:\t%q\n", fmt.Sprintf(inspect.Config.Cmd, args...))
		for i, arg := range inspect.Config.Args {
			if arg.Out {
				fmt.Fprintf(w, "\t  arg[%d]: output %d\n", i, arg.Index)
				continue
			}
			if syns[i] < 0 || arg.Fileset == nil {
				continue
			}
			indices := []int{i}
			for j := i; syns[j] > 0; j = syns[j] {
				indices = append(indices, syns[j])
			}
			for i := range indices {
				syns[i] = -1
			}
			strs := make([]string, len(indices))
			for i := range indices {
				strs[i] = fmt.Sprintf("arg[%d]", indices[i])
			}
			fmt.Fprintf(w, "\t  %s:\n", strings.Join(strs, ", "))
			c.printFileset(w, "\t    ", *arg.Fileset)
		}
	}
	if len(inspect.Commands) > 0 {
		fmt.Fprintln(w, "\ttop:")
		for _, cmd := range inspect.Commands {
			fmt.Fprintln(w, "\t\t", cmd)
		}
	}

	if result.Err != nil {
		fmt.Fprintf(w, "\terror:\t%s\n", result.Err)
	}
	if !result.Fileset.Empty() {
		fmt.Fprintf(w, "\tresult:\n")
		c.printFileset(w, "\t  ", result.Fileset)
	}
}

func (c *Cmd) printFileset(w io.Writer, prefix string, fs reflow.Fileset) {
	switch {
	case len(fs.List) > 0:
		for i := range fs.List {
			fmt.Fprintf(w, "%slist[%d]:\n", prefix, i)
			c.printFileset(w, prefix+"\t", fs.List[i])
		}
	case len(fs.Map) > 0:
		var keys []string
		for key := range fs.Map {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			file := fs.Map[key]
			fmt.Fprintf(w, "%s%s:\t%s (%s) assertions:%s\n", prefix, key, file.ID, data.Size(file.Size), file.Assertions)
		}
	}
}

func round(d time.Duration) time.Duration {
	return d - d%time.Second
}

type nameKind int

const (
	allocName nameKind = iota
	execName
	idName
	hostName
)

func (nk nameKind) String() string {
	switch nk {
	case allocName:
		return "alloc"
	case execName:
		return "exec"
	case idName:
		return "id"
	case hostName:
		return "hostname"
	}
	return "unknown"
}

type name struct {
	Kind        nameKind
	Hostname    string
	HostAndPort string
	AllocID     string
	ID          digest.Digest
}

func allocURI(n name) string {
	return strings.Join([]string{n.HostAndPort, n.AllocID}, "/")
}

func execPath(n name) string {
	if n.Kind == execName {
		return strings.Join([]string{"", n.AllocID, n.ID.Hex()}, "/")
	}
	panic(fmt.Sprintf("execPath not applicable for name: %v", n))
}

const objNameExamples = `
	9909853c                                                                    (requires taskdb)
	9909853c8cada5431400c5f89fe5658e139aea88cab8c1479a8c35c902b1cb49            (requires taskdb)
	sha256:9909853c8cada5431400c5f89fe5658e139aea88cab8c1479a8c35c902b1cb49     (requires taskdb)
	ec2-35-165-199-174.us-west-2.compute.amazonaws.com                          (works only with -reflowlet)
	ec2-35-165-199-174.us-west-2.compute.amazonaws.com:9000/bb97e35db4101030    (works only with -reflowlet)
	ec2-35-165-199-174.us-west-2.compute.amazonaws.com:9000/bb97e35db4101030/9909853c8cada5431400c5f89fe5658e139aea88cab8c1479a8c35c902b1cb49
`

var (
	hexRe     = regexp.MustCompile("^(sha256:)?[0-9a-f]+$")
	ec2HostRe = regexp.MustCompile("^ec2.*compute\\.amazonaws\\.com$")
)

// parseName parses a Reflow object name. See objNameExamples for examples.
func parseName(raw string) (n name, err error) {
	defer func() {
		if err != nil {
			err = errors.E("parseName", raw, err)
		}
	}()
	head, tail := peel(raw, "/")
	if tail == "" {
		switch {
		case hexRe.MatchString(head):
			n.Kind = idName
			n.ID, err = reflow.Digester.Parse(head)
		case ec2HostRe.MatchString(head):
			n.Kind, n.Hostname = hostName, head
		}
		return
	}
	if !hexRe.MatchString(head) {
		n.Hostname = strings.Split(head, ":")[0]
		n.HostAndPort = head
		head, tail = peel(tail, "/")
	}
	n.AllocID = head
	if tail == "" {
		n.Kind = allocName
		return
	}
	n.Kind = execName
	n.ID, err = reflow.Digester.Parse(tail)
	return
}

func peel(s, sep string) (head, tail string) {
	switch parts := strings.SplitN(s, sep, 2); len(parts) {
	case 1:
		return parts[0], ""
	case 2:
		return parts[0], parts[1]
	default:
		panic("bug")
	}
}
