package manager

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/hashicorp/consul-template/child"
	"github.com/hashicorp/consul-template/config"
	dep "github.com/hashicorp/consul-template/dependency"
	"github.com/hashicorp/consul-template/template"
	"github.com/hashicorp/consul-template/watch"
	"github.com/hashicorp/go-multierror"
	"github.com/mattn/go-shellwords"
	"github.com/pkg/errors"
)

const (
	// saneViewLimit is the number of views that we consider "sane" before we
	// warn the user that they might be DDoSing their Consul cluster.
	saneViewLimit = 128
)

// Runner responsible rendering Templates and invoking Commands.
type Runner struct {
	// ErrCh and DoneCh are channels where errors and finish notifications occur.
	ErrCh  chan error
	DoneCh chan struct{}

	// config is the Config that created this Runner. It is used internally to
	// construct other objects and pass data.
	config *config.Config

	// dry signals that output should be sent to stdout instead of committed to
	// disk. once indicates the runner should execute each template exactly one
	// time and then stop.
	dry, once bool

	// outStream and errStream are the io.Writer streams where the runner will
	// write information. These streams can be set using the SetOutStream()
	// and SetErrStream() functions.

	// inStream is the ioReader where the runner will read information.
	outStream, errStream io.Writer
	inStream             io.Reader

	// ctemplatesMap is a map of each template ID to the TemplateConfigs
	// that made it.
	ctemplatesMap map[string]config.TemplateConfigs

	// templates is the list of calculated templates.
	templates []*template.Template

	// renderEvents is a mapping of a template ID to the render event.
	renderEvents map[string]*RenderEvent

	// renderEventLock protects access into the renderEvents map
	renderEventsLock sync.RWMutex

	// renderedCh is used to signal that a template has been rendered
	renderedCh chan struct{}

	// dependencies is the list of dependencies this runner is watching.
	dependencies map[string]dep.Dependency

	// watcher is the watcher this runner is using.
	watcher *watch.Watcher

	// brain is the internal storage database of returned dependency data.
	brain *template.Brain

	// child is the child process under management. This may be nil if not running
	// in exec mode.
	child *child.Child

	// childLock is the internal lock around the child process.
	childLock sync.RWMutex

	// quiescenceMap is the map of templates to their quiescence timers.
	// quiescenceCh is the channel where templates report returns from quiescence
	// fires.
	quiescenceMap map[string]*quiescence
	quiescenceCh  chan *template.Template

	// dedup is the deduplication manager if enabled
	dedup *DedupManager

	// Env represents a custom set of environment variables to populate the
	// template and command runtime with. These environment variables will be
	// available in both the command's environment as well as the template's
	// environment.
	Env map[string]string
}

// RenderEvent captures the time and events that occurred for a template
// rendering.
type RenderEvent struct {
	// LastWouldRender marks the last time the template would have rendered.
	LastWouldRender time.Time

	// LastDidRender marks the last time the template was written to disk.
	LastDidRender time.Time
}

// NewRunner accepts a slice of TemplateConfigs and returns a pointer to the new
// Runner and any error that occurred during creation.
func NewRunner(config *config.Config, dry, once bool) (*Runner, error) {
	log.Printf("[INFO] (runner) creating new runner (dry: %v, once: %v)", dry, once)

	runner := &Runner{
		config: config,
		dry:    dry,
		once:   once,
	}

	if err := runner.init(); err != nil {
		return nil, err
	}

	return runner, nil
}

// Start begins the polling for this runner. Any errors that occur will cause
// this function to push an item onto the runner's error channel and the halt
// execution. This function is blocking and should be called as a goroutine.
func (r *Runner) Start() {
	log.Printf("[INFO] (runner) starting")

	// Create the pid before doing anything.
	if err := r.storePid(); err != nil {
		r.ErrCh <- err
		return
	}

	// Start the de-duplication manager
	var dedupCh <-chan struct{}
	if r.dedup != nil {
		if err := r.dedup.Start(); err != nil {
			r.ErrCh <- err
			return
		}
		dedupCh = r.dedup.UpdateCh()
	}

	// Setup the child process exit channel
	var childExitCh <-chan int

	// Fire an initial run to parse all the templates and setup the first-pass
	// dependencies. This also forces any templates that have no dependencies to
	// be rendered immediately (since they are already renderable).
	log.Printf("[DEBUG] (runner) running initial templates")
	if err := r.Run(); err != nil {
		r.ErrCh <- err
		return
	}

	for {
		// Enable quiescence for all templates if we have specified wait
		// intervals.
	NEXT_Q:
		for _, t := range r.templates {
			if _, ok := r.quiescenceMap[t.ID()]; ok {
				continue NEXT_Q
			}

			for _, c := range r.templateConfigsFor(t) {
				if *c.Wait.Enabled {
					log.Printf("[DEBUG] (runner) enabling template-specific quiescence for %q", t.ID())
					r.quiescenceMap[t.ID()] = newQuiescence(
						r.quiescenceCh, *c.Wait.Min, *c.Wait.Max, t)
					continue NEXT_Q
				}
			}

			if *r.config.Wait.Enabled {
				log.Printf("[DEBUG] (runner) enabling global quiescence for %q", t.ID())
				r.quiescenceMap[t.ID()] = newQuiescence(
					r.quiescenceCh, *r.config.Wait.Min, *r.config.Wait.Max, t)
				continue NEXT_Q
			}
		}

		// Warn the user if they are watching too many dependencies.
		if r.watcher.Size() > saneViewLimit {
			log.Printf("[WARN] (runner) watching %d dependencies - watching this "+
				"many dependencies could DDoS your consul cluster", r.watcher.Size())
		} else {
			log.Printf("[INFO] (runner) watching %d dependencies", r.watcher.Size())
		}

		if r.allTemplatesRendered() {
			// If an exec command was given and a command is not currently running,
			// spawn the child process for supervision.
			if *r.config.Exec.Command != "" {
				if r.child == nil {
					env := r.config.Exec.Env.Copy()
					env.Custom = append(r.childEnv(), env.Custom...)
					child, err := spawnChild(&spawnChildInput{
						Stdin:        r.inStream,
						Stdout:       r.outStream,
						Stderr:       r.errStream,
						Command:      config.StringVal(r.config.Exec.Command),
						Env:          env.Env(),
						ReloadSignal: config.SignalVal(r.config.Exec.ReloadSignal),
						KillSignal:   config.SignalVal(r.config.Exec.KillSignal),
						KillTimeout:  config.TimeDurationVal(r.config.Exec.KillTimeout),
						Splay:        config.TimeDurationVal(r.config.Exec.Splay),
					})
					if err != nil {
						r.ErrCh <- err
						r.childLock.Unlock()
						return
					}
					r.child = child
				}
				r.childLock.Unlock()

				// It's possible that we didn't start a process, in which case no
				// channel is returned. If we did get a new exitCh, that means a child
				// was spawned, so we need to watch a new exitCh. It is also possible
				// that during a run, the child process was restarted, which means a
				// new exit channel should be used.
				nexitCh := r.child.ExitCh()
				if nexitCh != nil {
					childExitCh = nexitCh
				}
			}

			// If we are running in once mode and all our templates are rendered,
			// then we should exit here.
			if r.once {
				log.Printf("[INFO] (runner) once mode and all templates rendered")

				if r.child != nil {
					r.stopDedup()
					r.stopWatcher()

					log.Printf("[INFO] (runner) waiting for child process to exit")
					select {
					case c := <-childExitCh:
						log.Printf("[INFO] (runner) child process died")
						r.ErrCh <- NewErrChildDied(c)
						return
					case <-r.DoneCh:
					}
				}

				r.Stop()
				return
			}
		}

	OUTER:
		select {
		case view := <-r.watcher.DataCh:
			// Receive this update
			r.Receive(view.Dependency, view.Data())

			// Drain all dependency data. Given a large number of dependencies, it is
			// feasible that we have data for more than one of them. Instead of
			// wasting CPU cycles rendering templates when we have more dependencies
			// waiting to be added to the brain, we drain the entire buffered channel
			// on the watcher and then reports when it is done receiving new data
			// which the parent select listens for.
			//
			// Please see https://github.com/hashicorp/consul-template/issues/168 for
			// more information about this optimization and the entire backstory.
			for {
				select {
				case view := <-r.watcher.DataCh:
					r.Receive(view.Dependency, view.Data())
				default:
					break OUTER
				}
			}

		case <-dedupCh:
			// We may get triggered by the de-duplication manager for either a change
			// in leadership (acquired or lost lock), or an update of data for a template
			// that we are watching.
			log.Printf("[INFO] (runner) watcher triggered by de-duplication manager")
			break OUTER

		case err := <-r.watcher.ErrCh:
			// If this is our own internal error, see if we should hard exit.
			if derr, ok := err.(*dep.FetchError); ok {
				log.Printf("[DEBUG] (runner) detected custom error type")
				if derr.ShouldExit() {
					log.Printf("[DEBUG] (runner) custom error asked for hard exit")
					r.ErrCh <- derr.OriginalError()
					return
				}
			}

			// Intentionally do not send the error back up to the runner. Eventually,
			// once Consul API implements errwrap and multierror, we can check the
			// "type" of error and conditionally alert back.
			//
			// if err.Contains(Something) {
			//   errCh <- err
			// }
			log.Printf("[ERR] (runner) watcher reported error: %s", err)
			if r.once {
				r.ErrCh <- err
				return
			}

		case tmpl := <-r.quiescenceCh:
			// Remove the quiescence for this template from the map. This will force
			// the upcoming Run call to actually evaluate and render the template.
			log.Printf("[INFO] (runner) received template %q from quiescence", tmpl.ID())
			delete(r.quiescenceMap, tmpl.ID())

		case c := <-childExitCh:
			log.Printf("[INFO] (runner) child process died")
			r.ErrCh <- NewErrChildDied(c)
			return

		case <-r.DoneCh:
			log.Printf("[INFO] (runner) received finish")
			return
		}

		// If we got this far, that means we got new data or one of the timers fired,
		// so attempt to re-render.
		if err := r.Run(); err != nil {
			r.ErrCh <- err
			return
		}
	}
}

// Stop halts the execution of this runner and its subprocesses.
func (r *Runner) Stop() {
	log.Printf("[INFO] (runner) stopping")
	r.stopDedup()
	r.stopWatcher()
	r.stopChild()

	if err := r.deletePid(); err != nil {
		log.Printf("[WARN] (runner) could not remove pid at %q: %s",
			r.config.PidFile, err)
	}

	close(r.DoneCh)
}

// TemplateRenderedCh returns a channel that will return the path of the
// template when it is rendered.
func (r *Runner) TemplateRenderedCh() <-chan struct{} {
	return r.renderedCh
}

// RenderEvents returns the render events for each template was rendered. The
// map is keyed by template ID.
func (r *Runner) RenderEvents() map[string]*RenderEvent {
	r.renderEventsLock.RLock()
	defer r.renderEventsLock.RUnlock()

	times := make(map[string]*RenderEvent, len(r.renderEvents))
	for k, v := range r.renderEvents {
		times[k] = v
	}
	return times
}

func (r *Runner) stopDedup() {
	if r.dedup != nil {
		log.Printf("[DEBUG] (runner) stopping de-duplication manager")
		r.dedup.Stop()
	} else {
		log.Printf("[DEBUG] (runner) de-duplication manager is not running")
	}
}

func (r *Runner) stopWatcher() {
	if r.watcher != nil {
		log.Printf("[DEBUG] (runner) stopping watcher")
		r.watcher.Stop()
	} else {
		log.Printf("[DEBUG] (runner) watcher is not running")
	}
}

func (r *Runner) stopChild() {
	r.childLock.RLock()
	defer r.childLock.RUnlock()

	if r.child != nil {
		log.Printf("[DEBUG] (runner) stopping child process")
		r.child.Stop()
	} else {
		log.Printf("[DEBUG] (runner) child is not running")
	}
}

// Receive accepts a Dependency and data for that dep. This data is
// cached on the Runner. This data is then used to determine if a Template
// is "renderable" (i.e. all its Dependencies have been downloaded at least
// once).
func (r *Runner) Receive(d dep.Dependency, data interface{}) {
	// Just because we received data, it does not mean that we are actually
	// watching for that data. How is that possible you may ask? Well, this
	// Runner's data channel is pooled, meaning it accepts multiple data views
	// before actually blocking. Whilest this runner is performing a Run() and
	// executing diffs, it may be possible that more data was pushed onto the
	// data channel pool for a dependency that we no longer care about.
	//
	// Accepting this dependency would introduce stale data into the brain, and
	// that is simply unacceptable. In fact, it is a fun little bug:
	//
	//     https://github.com/hashicorp/consul-template/issues/198
	//
	// and by "little" bug, I mean really big bug.
	if _, ok := r.dependencies[d.HashCode()]; ok {
		log.Printf("[DEBUG] (runner) receiving dependency %s", d.Display())
		r.brain.Remember(d, data)
	}
}

// Signal sends a signal to the child process, if it exists. Any errors that
// occur are returned.
func (r *Runner) Signal(s os.Signal) error {
	r.childLock.RLock()
	defer r.childLock.RUnlock()
	if r.child == nil {
		return nil
	}
	return r.child.Signal(s)
}

// Run iterates over each template in this Runner and conditionally executes
// the template rendering and command execution.
//
// The template is rendered atomicly. If and only if the template render
// completes successfully, the optional commands will be executed, if given.
// Please note that all templates are rendered **and then** any commands are
// executed.
func (r *Runner) Run() error {
	log.Printf("[INFO] (runner) running")

	var wouldRenderAny, renderedAny bool
	var commands []*config.TemplateConfig
	depsMap := make(map[string]dep.Dependency)

	for _, tmpl := range r.templates {
		log.Printf("[DEBUG] (runner) checking template %s", tmpl.ID())

		// Check if we are currently the leader instance
		isLeader := true
		if r.dedup != nil {
			isLeader = r.dedup.IsLeader(tmpl)
		}

		// If we are in once mode and this template was already rendered, move
		// onto the next one. We do not want to re-render the template if we are
		// in once mode, and we certainly do not want to re-run any commands.
		if r.once {
			r.renderEventsLock.RLock()
			_, rendered := r.renderEvents[tmpl.ID()]
			r.renderEventsLock.RUnlock()
			if rendered {
				log.Printf("[DEBUG] (runner) once mode and already rendered")
				continue
			}
		}

		// Attempt to render the template, returning any missing dependencies and
		// the rendered contents. If there are any missing dependencies, the
		// contents cannot be rendered or trusted!
		result, err := tmpl.Execute(&template.ExecuteInput{
			Brain: r.brain,
			Env:   r.childEnv(),
		})
		if err != nil {
			return err
		}

		// Grab the list of used and missing dependencies.
		missing, used := result.Missing, result.Used

		// Add the dependency to the list of dependencies for this runner.
		for _, d := range used {
			// If we've taken over leadership for a template, we may have data
			// that is cached, but not have the watcher. We must treat this as
			// missing so that we create the watcher and re-run the template.
			if isLeader && !r.watcher.Watching(d) {
				missing = append(missing, d)
			}
			if _, ok := depsMap[d.HashCode()]; !ok {
				depsMap[d.HashCode()] = d
			}
		}

		// Diff any missing dependencies the template reported with dependencies
		// the watcher is watching.
		var unwatched []dep.Dependency
		for _, d := range missing {
			if !r.watcher.Watching(d) {
				unwatched = append(unwatched, d)
			}
		}

		// If there are unwatched dependencies, start the watcher and move onto the
		// next one.
		if len(unwatched) > 0 {
			log.Printf("[INFO] (runner) was not watching %d dependencies", len(unwatched))
			for _, d := range unwatched {
				// If we are deduplicating, we must still handle non-sharable
				// dependencies, since those will be ignored.
				if isLeader || !d.CanShare() {
					r.watcher.Add(d)
				}
			}
			continue
		}

		// If the template is missing data for some dependencies then we are not
		// ready to render and need to move on to the next one.
		if len(missing) > 0 {
			log.Printf("[INFO] (runner) missing data for %d dependencies", len(missing))
			continue
		}

		// Trigger an update of the de-duplicaiton manager
		if r.dedup != nil && isLeader {
			if err := r.dedup.UpdateDeps(tmpl, used); err != nil {
				log.Printf("[ERR] (runner) failed to update dependency data for de-duplication: %v", err)
			}
		}

		// If quiescence is activated, start/update the timers and loop back around.
		// We do not want to render the templates yet.
		if q, ok := r.quiescenceMap[tmpl.ID()]; ok {
			q.tick()
			continue
		}

		// For each template configuration that is tied to this template, attempt to
		// render it to disk and accumulate commands for later use.
		for _, templateConfig := range r.templateConfigsFor(tmpl) {
			// Render the template, taking dry mode into account
			result, err := Render(&RenderInput{
				Backup:    config.BoolVal(templateConfig.Backup),
				Contents:  result.Output,
				Dry:       r.dry,
				DryStream: r.outStream,
				Path:      config.StringVal(templateConfig.Destination),
				Perms:     config.FileModeVal(templateConfig.Perms),
			})
			if err != nil {
				return errors.Wrap(err, fmt.Sprintf("error rendering %s", tmpl.ID()))
			}

			log.Printf("[DEBUG] (runner) WouldRender: %t, DidRender: %t",
				result.WouldRender, result.DidRender)

			// If we would have rendered this template (but we did not because the
			// contents were the same or something), we should consider this template
			// rendered even though the contents on disk have not been updated. We
			// will not fire commands unless the template was _actually_ rendered to
			// disk though.
			if result.WouldRender {
				// Make a note that we have rendered this template (required for once
				// mode and just generally nice for debugging purposes).
				r.markRenderTime(tmpl.ID(), false)

				// Record that at least one template would have been rendered.
				wouldRenderAny = true
			}

			// If we _actually_ rendered the template to disk, we want to run the
			// appropriate commands.
			if result.DidRender {
				// Record that at least one template was rendered.
				renderedAny = true

				// Store the render time
				r.markRenderTime(tmpl.ID(), true)

				if !r.dry {
					// If the template was rendered (changed) and we are not in dry-run mode,
					// aggregate commands, ignoring previously known commands
					//
					// Future-self Q&A: Why not use a map for the commands instead of an
					// array with an expensive lookup option? Well I'm glad you asked that
					// future-self! One of the API promises is that commands are executed
					// in the order in which they are provided in the TemplateConfig
					// definitions. If we inserted commands into a map, we would lose that
					// relative ordering and people would be unhappy.
					// if config.StringPresent(ctemplate.Command)
					if config.StringVal(templateConfig.Exec.Command) != "" && !commandExists(templateConfig, commands) {
						log.Println("[TRACE] appending command " + config.StringVal(templateConfig.Exec.Command))
						commands = append(commands, templateConfig)
					}
				}
			}
		}
	}

	// Check if we need to deliver any rendered signals
	if wouldRenderAny || renderedAny {
		// Send the signal that a template got rendered
		select {
		case r.renderedCh <- struct{}{}:
		default:
		}
	}

	// Perform the diff and update the known dependencies.
	r.diffAndUpdateDeps(depsMap)

	// Execute each command in sequence, collecting any errors that occur - this
	// ensures all commands execute at least once.
	var errs []error
	for _, t := range commands {
		env := t.Exec.Env.Copy()
		env.Custom = append(r.childEnv(), env.Custom...)
		if _, err := spawnChild(&spawnChildInput{
			Stdin:        r.inStream,
			Stdout:       r.outStream,
			Stderr:       r.errStream,
			Command:      config.StringVal(t.Exec.Command),
			Env:          env.Env(),
			Timeout:      config.TimeDurationVal(t.Exec.Timeout),
			ReloadSignal: config.SignalVal(t.Exec.ReloadSignal),
			KillSignal:   config.SignalVal(t.Exec.KillSignal),
			KillTimeout:  config.TimeDurationVal(t.Exec.KillTimeout),
			Splay:        config.TimeDurationVal(t.Exec.Splay),
		}); err != nil {
			errs = append(errs, err)
		}
	}

	// If we got this far and have a child process, we need to send the reload
	// signal to the child process.
	if renderedAny && r.child != nil {
		r.childLock.RLock()
		if err := r.child.Reload(); err != nil {
			errs = append(errs, err)
		}
		r.childLock.RUnlock()
	}

	// If any errors were returned, convert them to an ErrorList for human
	// readability.
	if len(errs) != 0 {
		var result *multierror.Error
		for _, err := range errs {
			result = multierror.Append(result, err)
		}
		return result.ErrorOrNil()
	}

	return nil
}

// init() creates the Runner's underlying data structures and returns an error
// if any problems occur.
func (r *Runner) init() error {
	// Ensure default configuration values
	r.config = config.DefaultConfig().Merge(r.config)

	// Print the final config for debugging
	result, err := json.MarshalIndent(r.config, "", "  ")
	if err != nil {
		return err
	}
	log.Printf("[DEBUG] (runner) final config (tokens suppressed):\n\n%s\n\n",
		result)

	// Create the clientset
	clients, err := newClientSet(r.config)
	if err != nil {
		return fmt.Errorf("runner: %s", err)
	}

	// Create the watcher
	watcher, err := newWatcher(r.config, clients, r.once)
	if err != nil {
		return fmt.Errorf("runner: %s", err)
	}
	r.watcher = watcher

	numTemplates := len(*r.config.Templates)
	templates := make([]*template.Template, 0, numTemplates)
	ctemplatesMap := make(map[string]config.TemplateConfigs)

	// Iterate over each TemplateConfig, creating a new Template resource for each
	// entry. Templates are parsed and saved, and a map of templates to their
	// config templates is kept so templates can lookup their commands and output
	// destinations.
	for _, ctmpl := range *r.config.Templates {
		tmpl, err := template.NewTemplate(&template.NewTemplateInput{
			Source:     config.StringVal(ctmpl.Source),
			Contents:   config.StringVal(ctmpl.Contents),
			LeftDelim:  config.StringVal(ctmpl.LeftDelim),
			RightDelim: config.StringVal(ctmpl.RightDelim),
		})
		if err != nil {
			return err
		}

		if _, ok := ctemplatesMap[tmpl.ID()]; !ok {
			templates = append(templates, tmpl)
		}

		if _, ok := ctemplatesMap[tmpl.ID()]; !ok {
			ctemplatesMap[tmpl.ID()] = make([]*config.TemplateConfig, 0, 1)
		}
		ctemplatesMap[tmpl.ID()] = append(ctemplatesMap[tmpl.ID()], ctmpl)
	}

	// Convert the map of templates (which was only used to ensure uniqueness)
	// back into an array of templates.
	r.templates = templates

	r.renderEvents = make(map[string]*RenderEvent, numTemplates)
	r.dependencies = make(map[string]dep.Dependency)

	r.renderedCh = make(chan struct{}, 1)

	r.ctemplatesMap = ctemplatesMap
	r.inStream = os.Stdin
	r.outStream = os.Stdout
	r.errStream = os.Stderr
	r.brain = template.NewBrain()

	r.ErrCh = make(chan error)
	r.DoneCh = make(chan struct{})

	r.quiescenceMap = make(map[string]*quiescence)
	r.quiescenceCh = make(chan *template.Template)

	// Setup the dedup manager if needed. This is
	if config.BoolVal(r.config.Dedup.Enabled) {
		if r.once {
			log.Printf("[INFO] (runner) disabling de-duplication in once mode")
		} else {
			r.dedup, err = NewDedupManager(r.config.Dedup, clients, r.brain, r.templates)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// diffAndUpdateDeps iterates through the current map of dependencies on this
// runner and stops the watcher for any deps that are no longer required.
//
// At the end of this function, the given depsMap is converted to a slice and
// stored on the runner.
func (r *Runner) diffAndUpdateDeps(depsMap map[string]dep.Dependency) {
	// Diff and up the list of dependencies, stopping any unneeded watchers.
	log.Printf("[INFO] (runner) diffing and updating dependencies")

	for key, d := range r.dependencies {
		if _, ok := depsMap[key]; !ok {
			log.Printf("[DEBUG] (runner) %s is no longer needed", d.Display())
			r.watcher.Remove(d)
			r.brain.Forget(d)
		} else {
			log.Printf("[DEBUG] (runner) %s is still needed", d.Display())
		}
	}

	r.dependencies = depsMap
}

// TemplateConfigFor returns the TemplateConfig for the given Template
func (r *Runner) templateConfigsFor(tmpl *template.Template) []*config.TemplateConfig {
	return r.ctemplatesMap[tmpl.ID()]
}

// TemplateConfigMapping returns a mapping between the template ID and the set
// of TemplateConfig represented by the template ID
func (r *Runner) TemplateConfigMapping() map[string][]config.TemplateConfig {
	m := make(map[string][]config.TemplateConfig, len(r.ctemplatesMap))

	for id, set := range r.ctemplatesMap {
		ctmpls := make([]config.TemplateConfig, len(set))
		m[id] = ctmpls
		for i, ctmpl := range set {
			ctmpls[i] = *ctmpl
		}
	}

	return m
}

// allTemplatesRendered returns true if all the templates in this Runner have
// been rendered at least one time.
func (r *Runner) allTemplatesRendered() bool {
	r.renderEventsLock.RLock()
	defer r.renderEventsLock.RUnlock()

	for _, tmpl := range r.templates {
		if _, rendered := r.renderEvents[tmpl.ID()]; !rendered {
			return false
		}
	}

	return true
}

// markRenderTime stores the render time for the given template. If didRender is
// true, it stores the time for the template having been rendered, otherwise it
// stores it as would have been rendered.
func (r *Runner) markRenderTime(tmplID string, didRender bool) {
	r.renderEventsLock.Lock()
	defer r.renderEventsLock.Unlock()

	// Get the current time
	now := time.Now()

	// Create the event for the template ID if it is the first time
	event, ok := r.renderEvents[tmplID]
	if !ok {
		event = &RenderEvent{}
		r.renderEvents[tmplID] = event
	}

	if didRender {
		event.LastDidRender = now
	} else {
		event.LastWouldRender = now
	}
}

// childEnv creates a map of environment variables for child processes to have
// access to configurations in Consul Template's configuration.
func (r *Runner) childEnv() []string {
	var m = make(map[string]string)

	if config.StringPresent(r.config.Consul) {
		m["CONSUL_HTTP_ADDR"] = config.StringVal(r.config.Consul)
	}

	if config.BoolVal(r.config.Auth.Enabled) {
		m["CONSUL_HTTP_AUTH"] = r.config.Auth.String()
	}

	m["CONSUL_HTTP_SSL"] = strconv.FormatBool(config.BoolVal(r.config.SSL.Enabled))
	m["CONSUL_HTTP_SSL_VERIFY"] = strconv.FormatBool(config.BoolVal(r.config.SSL.Verify))

	if config.StringPresent(r.config.Vault.Address) {
		m["VAULT_ADDR"] = config.StringVal(r.config.Vault.Address)
	}

	if !config.BoolVal(r.config.Vault.SSL.Verify) {
		m["VAULT_SKIP_VERIFY"] = "true"
	}

	if config.StringPresent(r.config.Vault.SSL.Cert) {
		m["VAULT_CLIENT_CERT"] = config.StringVal(r.config.Vault.SSL.Cert)
	}

	if config.StringPresent(r.config.Vault.SSL.Key) {
		m["VAULT_CLIENT_KEY"] = config.StringVal(r.config.Vault.SSL.Key)
	}

	if config.StringPresent(r.config.Vault.SSL.CaPath) {
		m["VAULT_CAPATH"] = config.StringVal(r.config.Vault.SSL.CaPath)
	}

	if config.StringPresent(r.config.Vault.SSL.CaCert) {
		m["VAULT_CACERT"] = config.StringVal(r.config.Vault.SSL.CaCert)
	}

	if config.StringPresent(r.config.Vault.SSL.ServerName) {
		m["VAULT_TLS_SERVER_NAME"] = config.StringVal(r.config.Vault.SSL.ServerName)
	}

	// Append runner-supplied env (this is supplied programatically).
	for k, v := range r.Env {
		m[k] = v
	}

	e := make([]string, 0, len(m))
	for k, v := range m {
		e = append(e, k+"="+v)
	}
	return e
}

// storePid is used to write out a PID file to disk.
func (r *Runner) storePid() error {
	path := config.StringVal(r.config.PidFile)
	if path == "" {
		return nil
	}

	log.Printf("[INFO] creating pid file at %q", path)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
	if err != nil {
		return fmt.Errorf("runner: could not open pid file: %s", err)
	}
	defer f.Close()

	pid := os.Getpid()
	_, err = f.WriteString(fmt.Sprintf("%d", pid))
	if err != nil {
		return fmt.Errorf("runner: could not write to pid file: %s", err)
	}
	return nil
}

// deletePid is used to remove the PID on exit.
func (r *Runner) deletePid() error {
	path := config.StringVal(r.config.PidFile)
	if path == "" {
		return nil
	}

	log.Printf("[DEBUG] removing pid file at %q", path)

	stat, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("runner: could not remove pid file: %s", err)
	}
	if stat.IsDir() {
		return fmt.Errorf("runner: specified pid file path is directory")
	}

	err = os.Remove(path)
	if err != nil {
		return fmt.Errorf("runner: could not remove pid file: %s", err)
	}
	return nil
}

// spawnChildInput is used as input to spawn a child process.
type spawnChildInput struct {
	Stdin        io.Reader
	Stdout       io.Writer
	Stderr       io.Writer
	Command      string
	Timeout      time.Duration
	Env          []string
	ReloadSignal os.Signal
	KillSignal   os.Signal
	KillTimeout  time.Duration
	Splay        time.Duration
}

// spawnChild spawns a child process with the given inputs and returns the
// resulting child.
func spawnChild(i *spawnChildInput) (*child.Child, error) {
	p := shellwords.NewParser()
	p.ParseEnv = true
	p.ParseBacktick = true
	args, err := p.Parse(i.Command)
	if err != nil {
		return nil, errors.Wrap(err, "failed parsing command")
	}

	child, err := child.New(&child.NewInput{
		Stdin:        i.Stdin,
		Stdout:       i.Stdout,
		Stderr:       i.Stderr,
		Command:      args[0],
		Args:         args[1:],
		Env:          i.Env,
		Timeout:      i.Timeout,
		ReloadSignal: i.ReloadSignal,
		KillSignal:   i.KillSignal,
		KillTimeout:  i.KillTimeout,
		Splay:        i.Splay,
	})
	if err != nil {
		return nil, errors.Wrap(err, "error creating child")
	}

	if err := child.Start(); err != nil {
		return nil, errors.Wrap(err, "error starting child")
	}
	return child, nil
}

// quiescence is an internal representation of a single template's quiescence
// state.
type quiescence struct {
	template *template.Template
	min      time.Duration
	max      time.Duration
	ch       chan *template.Template
	timer    *time.Timer
	deadline time.Time
}

// newQuiescence creates a new quiescence timer for the given template.
func newQuiescence(ch chan *template.Template, min, max time.Duration, t *template.Template) *quiescence {
	return &quiescence{
		template: t,
		min:      min,
		max:      max,
		ch:       ch,
	}
}

// tick updates the minimum quiescence timer.
func (q *quiescence) tick() {
	now := time.Now()

	// If this is the first tick, set up the timer and calculate the max
	// deadline.
	if q.timer == nil {
		q.timer = time.NewTimer(q.min)
		go func() {
			select {
			case <-q.timer.C:
				q.ch <- q.template
			}
		}()

		q.deadline = now.Add(q.max)
		return
	}

	// Snooze the timer for the min time, or snooze less if we are coming
	// up against the max time. If the timer has already fired and the reset
	// doesn't work that's ok because we guarantee that the channel gets our
	// template which means that we are obsolete and a fresh quiescence will
	// be set up.
	if now.Add(q.min).Before(q.deadline) {
		q.timer.Reset(q.min)
	} else if dur := q.deadline.Sub(now); dur > 0 {
		q.timer.Reset(dur)
	}
}

// Checks if a TemplateConfig with the given data exists in the list of Config
// Templates.
func commandExists(c *config.TemplateConfig, templates []*config.TemplateConfig) bool {
	needle := config.StringVal(c.Exec.Command)
	for _, t := range templates {
		if needle == config.StringVal(t.Exec.Command) {
			return true
		}
	}

	return false
}

// newClientSet creates a new client set from the given config.
func newClientSet(c *config.Config) (*dep.ClientSet, error) {
	clients := dep.NewClientSet()

	if err := clients.CreateConsulClient(&dep.CreateConsulClientInput{
		Address:      config.StringVal(c.Consul),
		Token:        config.StringVal(c.Token),
		AuthEnabled:  config.BoolVal(c.Auth.Enabled),
		AuthUsername: config.StringVal(c.Auth.Username),
		AuthPassword: config.StringVal(c.Auth.Password),
		SSLEnabled:   config.BoolVal(c.SSL.Enabled),
		SSLVerify:    config.BoolVal(c.SSL.Verify),
		SSLCert:      config.StringVal(c.SSL.Cert),
		SSLKey:       config.StringVal(c.SSL.Key),
		SSLCACert:    config.StringVal(c.SSL.CaCert),
		SSLCAPath:    config.StringVal(c.SSL.CaPath),
		ServerName:   config.StringVal(c.SSL.ServerName),
	}); err != nil {
		return nil, fmt.Errorf("runner: %s", err)
	}

	if err := clients.CreateVaultClient(&dep.CreateVaultClientInput{
		Address:     config.StringVal(c.Vault.Address),
		Token:       config.StringVal(c.Vault.Token),
		UnwrapToken: config.BoolVal(c.Vault.UnwrapToken),
		SSLEnabled:  config.BoolVal(c.Vault.SSL.Enabled),
		SSLVerify:   config.BoolVal(c.Vault.SSL.Verify),
		SSLCert:     config.StringVal(c.Vault.SSL.Cert),
		SSLKey:      config.StringVal(c.Vault.SSL.Key),
		SSLCACert:   config.StringVal(c.Vault.SSL.CaCert),
		SSLCAPath:   config.StringVal(c.Vault.SSL.CaPath),
		ServerName:  config.StringVal(c.Vault.SSL.ServerName),
	}); err != nil {
		return nil, fmt.Errorf("runner: %s", err)
	}

	return clients, nil
}

// newWatcher creates a new watcher.
func newWatcher(c *config.Config, clients *dep.ClientSet, once bool) (*watch.Watcher, error) {
	log.Printf("[INFO] (runner) creating Watcher")

	watcher, err := watch.NewWatcher(&watch.WatcherConfig{
		Clients:  clients,
		Once:     once,
		MaxStale: config.TimeDurationVal(c.MaxStale),
		RetryFunc: func(current time.Duration) time.Duration {
			return config.TimeDurationVal(c.Retry)
		},
		RenewVault: config.StringPresent(c.Vault.Token) && config.BoolVal(c.Vault.RenewToken),
	})
	if err != nil {
		return nil, err
	}

	return watcher, err
}
