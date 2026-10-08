// Package uploads is a collage plugin that receives file uploads, validates
// them by their content, stores them under random keys, and serves them back
// safely.
package uploads

import (
	"context"
	"errors"
	"fmt"

	"github.com/Elagoht/collage/pkg/collage"
)

// Name is the plugin's name, and the key of its section in the application's
// plugin configuration.
const Name = "elagoht/uploads"

const version = "0.1.0"

// defaultDir is where the disk store keeps files when the plugin was built
// without a store and nothing configures a directory.
const defaultDir = "var/uploads"

// Config is the plugin's configuration, read from its section of
// Config.PluginConfig:
//
//	{"elagoht/uploads": {"dir": "var/uploads"}}
//
// It is used only when the plugin was built without a store (New(nil) or
// NewWith).
type Config struct {
	// Dir is the directory of the disk store. Empty means "var/uploads"; a
	// section that sets it to "" explicitly is a startup error.
	Dir string `json:"dir"`
}

// Plugin is the uploads plugin.
type Plugin struct {
	store Store
	cfg   Config
}

// New returns the plugin storing files in store. With a nil store, Init
// builds a disk store from the configuration, as NewWith does.
func New(store Store) *Plugin { return &Plugin{store: store} }

// NewWith returns the plugin with a disk store in cfg.Dir, which the
// application's plugin configuration may override.
func NewWith(cfg Config) *Plugin { return &Plugin{cfg: cfg} }

// Name returns Name.
func (p *Plugin) Name() string { return Name }

// Version returns the plugin's version.
func (p *Plugin) Version() string { return version }

// Init keeps a store given to New. Otherwise it reads the configuration over
// the directory NewWith was given ("var/uploads" when none was) and builds the
// disk store there. A configuration that leaves no directory is an error.
func (p *Plugin) Init(_ context.Context, host collage.Host) error {
	if p.store != nil {
		return nil
	}
	defaults := p.cfg
	if defaults.Dir == "" {
		defaults.Dir = defaultDir
	}
	cfg, err := collage.PluginConfig(host, defaults)
	if err != nil {
		return err
	}
	if cfg.Dir == "" {
		return errors.New("elagoht/uploads: no store: pass one to New, or set \"dir\" in the plugin configuration")
	}
	p.cfg = cfg
	p.store = Dir(cfg.Dir)
	return nil
}

// Shutdown does nothing: the plugin holds nothing open between requests.
func (p *Plugin) Shutdown(context.Context) error { return nil }

// Store returns the store files are kept in: the one given to New, or the
// disk store Init built. It is nil before Init when none was given.
func (p *Plugin) Store() Store { return p.store }

var _ collage.Plugin = (*Plugin)(nil)

var (
	// ErrTooLarge is a file over its field's MaxSize, or non-file values over
	// 64 KiB in total.
	ErrTooLarge = errors.New("elagoht/uploads: too large")
	// ErrType is a file whose sniffed type is not in its field's Types.
	ErrType = errors.New("elagoht/uploads: type not allowed")
	// ErrTooMany is a field carrying more than its MaxFiles files.
	ErrTooMany = errors.New("elagoht/uploads: too many files")
	// ErrUnexpectedField is a file in a field Receive was not given.
	ErrUnexpectedField = errors.New("elagoht/uploads: unexpected file field")
	// ErrAlreadyRead is a second Receive on one request.
	ErrAlreadyRead = errors.New("elagoht/uploads: request body already read")
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
