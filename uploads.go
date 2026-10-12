// Package uploads is a collage plugin that receives file uploads, validates
// them by their content, stores them under random keys, and serves them back
// safely.
package uploads

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/Elagoht/collage/pkg/collage"
)

// Name is the plugin's name, and the key of its section in the application's
// plugin configuration.
const Name = "elagoht/uploads"

const version = "0.2.0"

// defaultDir is where the disk store keeps files when the plugin was built
// without a store and nothing configures a directory.
const defaultDir = "var/uploads"

// Options configures the plugin. Dir and CacheControl are read from its
// section of Config.PluginConfig too, over the values given here:
//
//	{"elagoht/uploads": {"dir": "var/uploads", "cacheControl": "private, max-age=0"}}
//
// The zero Options keeps files on disk in "var/uploads".
type Options struct {
	// Store keeps the files. Nil builds a disk store in Dir at Init.
	Store Store `json:"-"`
	// Dir is the directory of the disk store, used when Store is nil; default
	// "var/uploads". A section that sets "dir" to "" explicitly is a startup
	// error.
	Dir string `json:"dir"`
	// CacheControl is the Cache-Control header Handler sends with every file;
	// default "private, max-age=0": a browser may keep the file but must
	// revalidate it, and a shared cache must not keep it at all.
	CacheControl string `json:"cacheControl"`
}

// Plugin is the uploads plugin.
type Plugin struct {
	store Store
	opts  Options
	// log is the host's logger, which Handler reports store errors to.
	log *slog.Logger
}

// New returns the plugin with opts as its starting point, which the
// application's own configuration is then decoded over.
func New(opts Options) *Plugin { return &Plugin{store: opts.Store, opts: opts} }

// Name returns Name.
func (p *Plugin) Name() string { return Name }

// Version returns the plugin's version.
func (p *Plugin) Version() string { return version }

// Init reads the configuration over the options New was given and keeps the
// host's logger for Handler. With no Store, it builds the disk store in Dir
// ("var/uploads" when none was given); a configuration that leaves no
// directory is an error.
func (p *Plugin) Init(_ context.Context, host collage.Host) error {
	p.log = host.Logger()
	defaults := p.opts
	if defaults.Dir == "" {
		defaults.Dir = defaultDir
	}
	opts, err := collage.PluginConfig(host, defaults)
	if err != nil {
		return err
	}
	p.opts = opts
	if p.store != nil {
		return nil
	}
	if opts.Dir == "" {
		return errors.New("uploads: no store: set Options.Store, or \"dir\" in the plugin configuration")
	}
	p.store = Dir(opts.Dir)
	return nil
}

// Shutdown does nothing: the plugin holds nothing open between requests.
func (p *Plugin) Shutdown(context.Context) error { return nil }

// Store returns the store files are kept in: Options.Store, or the
// disk store Init built. It is nil before Init when none was given.
func (p *Plugin) Store() Store { return p.store }

var _ collage.Plugin = (*Plugin)(nil)

var (
	// ErrTooLarge is a file over its field's MaxSize, or non-file values over
	// 64 KiB in total.
	ErrTooLarge = errors.New("uploads: too large")
	// ErrType is a file whose sniffed type is not in its field's Types.
	ErrType = errors.New("uploads: type not allowed")
	// ErrTooMany is a field carrying more than its MaxFiles files.
	ErrTooMany = errors.New("uploads: too many files")
	// ErrUnexpectedField is a file in a field Receive was not given.
	ErrUnexpectedField = errors.New("uploads: unexpected file field")
	// ErrAlreadyRead is a second Receive on one request.
	ErrAlreadyRead = errors.New("uploads: request body already read")
)

// Error is a rule a submission broke, and the field that broke it. Err is one
// of ErrTooLarge, ErrType, ErrTooMany or ErrUnexpectedField, so an action can
// show it beside the field as a form error.
type Error struct {
	Field string
	Err   error
}

func (e *Error) Error() string {
	return fmt.Sprintf("%v (field %q)", e.Err, e.Field)
}

func (e *Error) Unwrap() error { return e.Err }
