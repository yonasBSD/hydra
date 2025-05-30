// Copyright © 2022 Ory Corp
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ory/x/popx"
	"github.com/ory/x/servicelocatorx"

	"github.com/pkg/errors"

	"github.com/ory/x/configx"

	"github.com/ory/x/cmdx"

	"github.com/spf13/cobra"

	"github.com/ory/hydra/v2/driver"
	"github.com/ory/hydra/v2/driver/config"
	"github.com/ory/hydra/v2/persistence"
	"github.com/ory/x/flagx"
)

type MigrateHandler struct {
	slOpts []servicelocatorx.Option
	dOpts  []driver.OptionsModifier
	cOpts  []configx.OptionModifier
}

func newMigrateHandler(slOpts []servicelocatorx.Option, dOpts []driver.OptionsModifier, cOpts []configx.OptionModifier) *MigrateHandler {
	return &MigrateHandler{
		slOpts: slOpts,
		dOpts:  dOpts,
		cOpts:  cOpts,
	}
}

const (
	genericDialectKey = "any"
)

func fragmentHeader() []byte {
	return fmt.Appendf(nil, "-- Migration generated by the command below; DO NOT EDIT.\n-- %s\n", strings.Join(os.Args, " "))
}

func blankFragment() []byte {
	return fmt.Appendf(nil, "-- This is a blank migration. It is generated to ensure that all dialects are represented in the migration files.\n-- %s\n", strings.Join(os.Args, " "))
}

var mrx = regexp.MustCompile(`^(\d{14})000000_([^.]+)(\.[a-z0-9]+)?\.(up|down)\.sql$`)

type migration struct {
	Path      string
	ID        string
	Name      string
	Dialect   string
	Direction string
}

type migrationGroup struct {
	ID                    string
	Name                  string
	Children              []*migration
	fallbackUpMigration   *migration
	fallbackDownMigration *migration
}

func (m *migration) ReadSource(fs fs.FS) ([]byte, error) {
	f, err := fs.Open(m.Path)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	defer f.Close()
	return io.ReadAll(f)
}

func (m migration) generateMigrationFragments(source []byte) ([][]byte, error) {
	chunks := bytes.Split(source, []byte("--split"))
	if len(chunks) < 1 {
		return nil, errors.New("no migration chunks found")
	}
	for i := range chunks {
		chunks[i] = append(fragmentHeader(), chunks[i]...)
	}
	return chunks, nil
}

func (mg migrationGroup) fragmentName(m *migration, i int) string {
	if m.Dialect == genericDialectKey {
		return fmt.Sprintf("%s%06d_%s.%s.sql", mg.ID, i, mg.Name, m.Direction)
	} else {
		return fmt.Sprintf("%s%06d_%s.%s.%s.sql", mg.ID, i, mg.Name, m.Dialect, m.Direction)
	}
}

// GenerateSQL splits the migration sources into chunks and writes them to the
// target directory.
func (mg migrationGroup) generateSQL(sourceFS fs.FS, target string) error {
	ms := mg.Children
	if mg.fallbackDownMigration != nil {
		ms = append(ms, mg.fallbackDownMigration)
	}
	if mg.fallbackUpMigration != nil {
		ms = append(ms, mg.fallbackUpMigration)
	}
	dialectFragmentCounts := map[string]int{}
	maxFragmentCount := -1
	for _, m := range ms {
		source, err := m.ReadSource(sourceFS)
		if err != nil {
			return errors.WithStack(err)
		}

		fragments, err := m.generateMigrationFragments(source)
		dialectFragmentCounts[m.Dialect] = len(fragments)
		if maxFragmentCount < len(fragments) {
			maxFragmentCount = len(fragments)
		}
		if err != nil {
			return errors.Errorf("failed to process %s: %s", m.Path, err.Error())
		}
		for i, fragment := range fragments {
			dst := filepath.Join(target, mg.fragmentName(m, i))
			if err = os.WriteFile(dst, fragment, 0600); err != nil {
				return errors.WithStack(errors.Errorf("failed to write file %s", dst))
			}
		}
	}
	for _, m := range ms {
		for i := dialectFragmentCounts[m.Dialect]; i < maxFragmentCount; i += 1 {
			dst := filepath.Join(target, mg.fragmentName(m, i))
			if err := os.WriteFile(dst, blankFragment(), 0600); err != nil {
				return errors.WithStack(errors.Errorf("failed to write file %s", dst))
			}
		}
	}
	return nil
}

func parseMigration(filename string) (*migration, error) {
	matches := mrx.FindAllStringSubmatch(filename, -1)
	if matches == nil {
		return nil, errors.Errorf("failed to parse migration filename %s; %s does not match pattern ", filename, mrx.String())
	}
	if len(matches) != 1 && len(matches[0]) != 5 {
		return nil, errors.Errorf("invalid migration %s; expected %s", filename, mrx.String())
	}
	dialect := matches[0][3]
	if dialect == "" {
		dialect = genericDialectKey
	} else {
		dialect = dialect[1:]
	}
	return &migration{
		Path:      filename,
		ID:        matches[0][1],
		Name:      matches[0][2],
		Dialect:   dialect,
		Direction: matches[0][4],
	}, nil
}

func readMigrations(migrationSourceFS fs.FS, expectedDialects []string) (map[string]*migrationGroup, error) {
	mgs := make(map[string]*migrationGroup)
	err := fs.WalkDir(migrationSourceFS, ".", func(p string, d fs.DirEntry, err2 error) error {
		if err2 != nil {
			fmt.Println("Warning: unexpected error " + err2.Error())
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if p != filepath.Base(p) {
			fmt.Println("Warning: ignoring nested file " + p)
			return nil
		}

		m, err := parseMigration(p)
		if err != nil {
			return err
		}

		if _, ok := mgs[m.ID]; !ok {
			mgs[m.ID] = &migrationGroup{
				ID:       m.ID,
				Name:     m.Name,
				Children: nil,
			}
		}

		if m.Dialect == genericDialectKey && m.Direction == "up" {
			mgs[m.ID].fallbackUpMigration = m
		} else if m.Dialect == genericDialectKey && m.Direction == "down" {
			mgs[m.ID].fallbackDownMigration = m
		} else {
			mgs[m.ID].Children = append(mgs[m.ID].Children, m)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	if len(expectedDialects) == 0 {
		return mgs, nil
	}

	eds := make(map[string]struct{})
	for i := range expectedDialects {
		eds[expectedDialects[i]] = struct{}{}
	}
	for _, mg := range mgs {
		expect := make(map[string]struct{})
		for _, m := range mg.Children {
			if _, ok := eds[m.Dialect]; !ok {
				return nil, errors.Errorf("unexpected dialect %s in filename %s", m.Dialect, m.Path)
			}

			expect[m.Dialect+"."+m.Direction] = struct{}{}
		}
		for _, d := range expectedDialects {
			if _, ok := expect[d+".up"]; !ok && mg.fallbackUpMigration == nil {
				return nil, errors.Errorf("dialect %s not found for up migration %s; use --dialects=\"\" to disable dialect validation", d, mg.ID)
			}
			if _, ok := expect[d+".down"]; !ok && mg.fallbackDownMigration == nil {
				return nil, errors.Errorf("dialect %s not found for down migration %s; use --dialects=\"\" to disable dialect validation", d, mg.ID)
			}
		}
	}

	return mgs, nil
}

func (h *MigrateHandler) MigrateGen(cmd *cobra.Command, args []string) {
	cmdx.ExactArgs(cmd, args, 2)
	expectedDialects := flagx.MustGetStringSlice(cmd, "dialects")

	sourceDir := args[0]
	targetDir := args[1]
	sourceFS := os.DirFS(sourceDir)
	mgs, err := readMigrations(sourceFS, expectedDialects)
	if err != nil {
		fmt.Println(err.Error())
		os.Exit(1)
	}
	for _, mg := range mgs {
		err = mg.generateSQL(sourceFS, targetDir)
		if err != nil {
			fmt.Println(err.Error())
			os.Exit(1)
		}
	}

	os.Exit(0)
}

func (h *MigrateHandler) makePersister(cmd *cobra.Command, args []string) (p persistence.Persister, err error) {
	var d driver.Registry

	if flagx.MustGetBool(cmd, "read-from-env") {
		d, err = driver.New(
			cmd.Context(),
			servicelocatorx.NewOptions(),
			append([]driver.OptionsModifier{
				driver.WithOptions(
					configx.SkipValidation(),
					configx.WithFlags(cmd.Flags())),
				driver.DisableValidation(),
				driver.DisablePreloading(),
				driver.SkipNetworkInit(),
			}, h.dOpts...))
		if err != nil {
			return nil, err
		}
		if len(d.Config().DSN()) == 0 {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "When using flag -e, environment variable DSN must be set.")
			return nil, cmdx.FailSilently(cmd)
		}
	} else {
		if len(args) != 1 {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Please provide the database URL.")
			return nil, cmdx.FailSilently(cmd)
		}
		d, err = driver.New(
			cmd.Context(),
			servicelocatorx.NewOptions(),
			append([]driver.OptionsModifier{
				driver.WithOptions(
					configx.WithFlags(cmd.Flags()),
					configx.SkipValidation(),
					configx.WithValue(config.KeyDSN, args[0]),
				),
				driver.DisableValidation(),
				driver.DisablePreloading(),
				driver.SkipNetworkInit(),
			}, h.dOpts...))
		if err != nil {
			return nil, err
		}
	}
	return d.Persister(), nil
}

func (h *MigrateHandler) MigrateSQLUp(cmd *cobra.Command, args []string) (err error) {
	p, err := h.makePersister(cmd, args)
	if err != nil {
		return err
	}
	return popx.MigrateSQLUp(cmd, p)
}

func (h *MigrateHandler) MigrateSQLDown(cmd *cobra.Command, args []string) (err error) {
	p, err := h.makePersister(cmd, args)
	if err != nil {
		return err
	}
	return popx.MigrateSQLDown(cmd, p)
}

func (h *MigrateHandler) MigrateStatus(cmd *cobra.Command, args []string) error {
	p, err := h.makePersister(cmd, args)
	if err != nil {
		return err
	}
	return popx.MigrateStatus(cmd, p)
}
