package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/types"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kyleconroy/sqlc/internal/pg"
)

const errMessageNoVersion = `The configuration file must have a version number.
Set the version to 1 at the top of sqlc.json:

{
  "version": "1"
  ...
}
`

const errMessageUnknownVersion = `The configuration file has an invalid version number.
The only supported version is "1".
`

const errMessageNoPackages = `No packages are configured`

type GenerateSettings struct {
	Version   string            `json:"version"`
	Packages  []PackageSettings `json:"packages"`
	Overrides []Override        `json:"overrides,omitempty"`
	Rename    map[string]string `json:"rename,omitempty"`
}

type Engine string

const (
	EngineMySQL      Engine = "mysql"
	EnginePostgreSQL Engine = "postgresql"
)

type PackageSettings struct {
	Name                string     `json:"name"`
	Engine              Engine     `json:"engine,omitempty"`
	Path                string     `json:"path"`
	Schema              string     `json:"schema"`
	Queries             string     `json:"queries"`
	EmitInterface       bool       `json:"emit_interface"`
	EmitJSONTags        bool       `json:"emit_json_tags"`
	EmitPreparedQueries bool       `json:"emit_prepared_queries"`
	Overrides           []Override `json:"overrides"`
}

type Override struct {
	// name of the golang type to use, e.g. `github.com/segmentio/ksuid.KSUID`
	GoType string `json:"go_type"`

	// fully qualified name of the Go type, e.g. `github.com/segmentio/ksuid.KSUID`
	DBType                  string `json:"db_type"`
	Deprecated_PostgresType string `json:"postgres_type"`

	// for global overrides only when two different engines are in use
	Engine Engine `json:"engine,omitempty"`

	// True if the GoType should override if the maching postgres type is nullable
	Null bool `json:"null"`

	// fully qualified name of the column, e.g. `accounts.id`
	Column string `json:"column"`

	ColumnName  string
	Table       pg.FQN
	GoTypeName  string
	GoPackage   string
	GoBasicType bool
}

func (c *GenerateSettings) ValidateGlobalOverrides() error {
	engines := map[Engine]struct{}{}
	for _, pkg := range c.Packages {
		if _, ok := engines[pkg.Engine]; !ok {
			engines[pkg.Engine] = struct{}{}
		}
	}

	usesMultipleEngines := len(engines) > 1
	for _, oride := range c.Overrides {
		if usesMultipleEngines && oride.Engine == "" {
			return fmt.Errorf(`the "engine" field is required for global type overrides because your configuration uses multiple database engines`)
		}
	}
	return nil
}

func (o *Override) Parse() error {

	// validate deprecated postgres_type field
	if o.Deprecated_PostgresType != "" {
		fmt.Fprintf(os.Stderr, "WARNING: \"postgres_type\" is deprecated. Instead, use \"db_type\" to specify a type override.\n")
		if o.DBType != "" {
			return fmt.Errorf(`Type override configurations cannot have "db_type" and "postres_type" together. Use "db_type" alone`)
		}
		o.DBType = o.Deprecated_PostgresType
	}

	// validate option combinations
	switch {
	case o.Column != "" && o.DBType != "":
		return fmt.Errorf("Override specifying both `column` (%q) and `db_type` (%q) is not valid.", o.Column, o.DBType)
	case o.Column == "" && o.DBType == "":
		return fmt.Errorf("Override must specify one of either `column` or `db_type`")
	}

	// validate Column
	if o.Column != "" {
		colParts := strings.Split(o.Column, ".")
		switch len(colParts) {
		case 2:
			o.ColumnName = colParts[1]
			o.Table = pg.FQN{Schema: "public", Rel: colParts[0]}
		case 3:
			o.ColumnName = colParts[2]
			o.Table = pg.FQN{Schema: colParts[0], Rel: colParts[1]}
		case 4:
			o.ColumnName = colParts[3]
			o.Table = pg.FQN{Catalog: colParts[0], Schema: colParts[1], Rel: colParts[2]}
		default:
			return fmt.Errorf("Override `column` specifier %q is not the proper format, expected '[catalog.][schema.]colname.tablename'", o.Column)
		}
	}

	// validate GoType
	lastDot := strings.LastIndex(o.GoType, ".")
	lastSlash := strings.LastIndex(o.GoType, "/")
	typename := o.GoType
	if lastDot == -1 && lastSlash == -1 {
		// if the type name has no slash and no dot, validate that the type is a basic Go type
		var found bool
		for _, typ := range types.Typ {
			info := typ.Info()
			if info == 0 {
				continue
			}
			if info&types.IsUntyped != 0 {
				continue
			}
			if typename == typ.Name() {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("Package override `go_type` specifier %q is not a Go basic type e.g. 'string'", o.GoType)
		}
		o.GoBasicType = true
	} else {
		// assume the type lives in a Go package
		if lastDot == -1 {
			return fmt.Errorf("Package override `go_type` specifier %q is not the proper format, expected 'package.type', e.g. 'github.com/segmentio/ksuid.KSUID'", o.GoType)
		}
		if lastSlash == -1 {
			return fmt.Errorf("Package override `go_type` specifier %q is not the proper format, expected 'package.type', e.g. 'github.com/segmentio/ksuid.KSUID'", o.GoType)
		}
		typename = o.GoType[lastSlash+1:]
		if strings.HasPrefix(typename, "go-") {
			// a package name beginning with "go-" will give syntax errors in
			// generated code. We should do the right thing and get the actual
			// import name, but in lieu of that, stripping the leading "go-" may get
			// us what we want.
			typename = typename[len("go-"):]
		}
		if strings.HasSuffix(typename, "-go") {
			typename = typename[:len(typename)-len("-go")]
		}
		o.GoPackage = o.GoType[:lastDot]
	}
	o.GoTypeName = typename
	isPointer := o.GoType[0] == '*'
	if isPointer {
		o.GoPackage = o.GoPackage[1:]
		o.GoTypeName = "*" + o.GoTypeName
	}

	return nil
}

var ErrMissingVersion = errors.New("no version number")
var ErrUnknownVersion = errors.New("invalid version number")
var ErrNoPackages = errors.New("no packages")
var ErrNoPackageName = errors.New("missing package name")
var ErrNoPackagePath = errors.New("missing package path")

func ParseConfig(rd io.Reader) (GenerateSettings, error) {
	dec := json.NewDecoder(rd)
	dec.DisallowUnknownFields()
	var config GenerateSettings
	if err := dec.Decode(&config); err != nil {
		return config, err
	}
	if config.Version == "" {
		return config, ErrMissingVersion
	}
	if config.Version != "1" {
		return config, ErrUnknownVersion
	}
	if len(config.Packages) == 0 {
		return config, ErrNoPackages
	}
	if err := config.ValidateGlobalOverrides(); err != nil {
		return config, err
	}
	for i := range config.Overrides {
		if err := config.Overrides[i].Parse(); err != nil {
			return config, err
		}
	}
	for j := range config.Packages {
		if config.Packages[j].Path == "" {
			return config, ErrNoPackagePath
		}
		for i := range config.Packages[j].Overrides {
			if err := config.Packages[j].Overrides[i].Parse(); err != nil {
				return config, err
			}
		}
		if config.Packages[j].Name == "" {
			config.Packages[j].Name = filepath.Base(config.Packages[j].Path)
		}
		if config.Packages[j].Engine == "" {
			config.Packages[j].Engine = EnginePostgreSQL
		}
	}
	return config, nil
}

type CombinedSettings struct {
	Global    GenerateSettings
	Package   PackageSettings
	Overrides []Override
}

func Combine(gen GenerateSettings, pkg PackageSettings) CombinedSettings {
	return CombinedSettings{
		Global:    gen,
		Package:   pkg,
		Overrides: append(gen.Overrides, pkg.Overrides...),
	}
}
