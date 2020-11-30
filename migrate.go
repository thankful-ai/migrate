package migrate

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/pkg/errors"
)

// version of the migrate tool's database schema.
const version = 1

var spaces = regexp.MustCompile(`\s+`)

type Migrate struct {
	Migrations []Migration
	Files      []*file

	db  Store
	log Logger
	idx int
}

type file struct {
	Info     os.FileInfo
	fullpath string
}

type Migration struct {
	Filename string
	Checksum string
	Content  string
	fullpath string
}

var regexNum = regexp.MustCompile(`^\d+`)

type DBType string

const (
	DBTypeMySQL    DBType = "mysql"
	DBTypeMariaDB  DBType = "mariadb"
	DBTypePostgres DBType = "postgres"
	DBTypeSQLite   DBType = "sqlite"
)

func New(
	db Store,
	log Logger,
	dbt DBType,
	dir, skip string,
) (*Migrate, error) {
	m := &Migrate{db: db, log: log}

	// Get files in migration dir and sort them
	var err error
	m.Files, err = readDir(dir, dbt)
	if err != nil {
		return nil, errors.Wrap(err, "get migrations")
	}
	if err = sortFiles(m.Files); err != nil {
		return nil, errors.Wrap(err, "sort")
	}

	// Create meta tables if we need to, so we can store the migration
	// state in the db itself
	if err = db.CreateMetaIfNotExists(); err != nil {
		return nil, errors.Wrap(err, "create meta table")
	}
	if err = db.CreateMetaCheckpointsIfNotExists(); err != nil {
		return nil, errors.Wrap(err, "create meta checkpoints table")
	}
	curVersion, err := db.CreateMetaVersionIfNotExists(version)
	if err != nil {
		return nil, errors.Wrap(err, "create meta version table")
	}

	// Migrate the database schema to match the tool's expectations
	// automatically
	if curVersion > version {
		return nil, errors.New("must upgrade migrate: go get -u github.com/thankful-ai/migrate")
	}
	if curVersion < 1 {
		tmpMigrations, err := migrationsFromFiles(m)
		if err != nil {
			return nil, errors.Wrap(err, "migrations from files")
		}
		if err = db.UpgradeToV1(tmpMigrations); err != nil {
			return nil, errors.Wrap(err, "upgrade to v1")
		}
		curVersion = 1
	}

	// If skip, then we record the migrations but do not perform them. This
	// enables you to start using this package on an existing database
	if skip != "" {
		m.idx, err = m.skip(skip)
		if err != nil {
			return nil, errors.Wrap(err, "skip ahead")
		}
		m.log.Println("skipped ahead")
	}

	// Get all migrations
	m.Migrations, err = db.GetMigrations()
	if err != nil {
		return nil, errors.Wrap(err, "get migrations")
	}

	// Fill in migration fullpath field based on the db type.
	overrides, err := getOverrideSet(dir, dbt)
	if err != nil {
		return nil, fmt.Errorf("get override set: %w", err)
	}
	for i, mg := range m.Migrations {
		override, exist := overrides[mg.Filename]
		if exist {
			m.Migrations[i].fullpath = override.fullpath
		} else {
			m.Migrations[i].fullpath = filepath.Join(dir, mg.Filename)
		}
	}
	if err = m.validHistory(); err != nil {
		return nil, err
	}
	return m, nil
}

// Migrate all files in the directory. This function reports whether any
// migration took place.
func (m *Migrate) Migrate() (bool, error) {
	var migrated bool
	for i := len(m.Migrations); i < len(m.Files); i++ {
		fi := m.Files[i]
		if err := m.migrateFile(fi); err != nil {
			return false, errors.Wrap(err, "migrate file")
		}
		m.log.Println("migrated", fi.Info.Name())
		migrated = true
	}
	return migrated, nil
}

func (m *Migrate) validHistory() error {
	for i := len(m.Files); i < len(m.Migrations); i++ {
		m.log.Printf("missing already-run migration %q\n", m.Migrations[i])
	}
	if len(m.Files) < len(m.Migrations) {
		return errors.New("cannot continue with missing migrations")
	}
	for i := m.idx; i < len(m.Migrations); i++ {
		mg := m.Migrations[i]
		if mg.Filename != m.Files[i].Info.Name() {
			m.log.Printf("\n%s was added to history before %s.\n",
				m.Files[i].Info.Name(), mg.Filename)
			return errors.New("failed to migrate. migrations must be appended")
		}
		if err := m.checkHash(mg); err != nil {
			return errors.Wrap(err, "check hash")
		}
	}
	return nil
}

func (m *Migrate) checkHash(mg Migration) error {
	fi, err := os.Open(mg.fullpath)
	if err != nil {
		return err
	}
	defer fi.Close()
	_, check, err := computeChecksum(fi)
	if err != nil {
		return err
	}
	if check != mg.Checksum {
		m.log.Println("comparing", check, mg.Checksum)
		return fmt.Errorf("checksum does not match %s. has the file changed?",
			mg.Filename)
	}
	return nil
}

func (m *Migrate) migrateFile(fi *file) error {
	byt, err := ioutil.ReadFile(fi.fullpath)
	if err != nil {
		return err
	}

	// Split commands and remove comments at the start of lines
	cmds := strings.Split(string(byt), ";")

	// For postgresql specifically, some statements may have multiple `;`
	// such as when creating functions. Join those together.
	newCmds := []string{}
	var keepGoing bool
	for _, c := range cmds {
		lowC := strings.ToLower(c)
		if strings.Contains(lowC, "returns trigger as") {
			keepGoing = true
			newCmds = append(newCmds, c+";")
			continue
		}
		if keepGoing {
			newCmds[len(newCmds)-1] += c
			if !strings.Contains(lowC, "plpgsql") {
				newCmds[len(newCmds)-1] += ";"
				continue
			}
			keepGoing = false
			continue
		}
		newCmds = append(newCmds, c)
	}
	if keepGoing {
		return errors.New("unexpected exit, missing 'plpgsql'")
	}
	cmds = newCmds

	filteredCmds := []string{}
	for _, cmd := range cmds {
		cmd = strings.TrimSpace(cmd)
		if len(cmd) > 0 && !strings.HasPrefix(cmd, "--") {
			filteredCmds = append(filteredCmds, cmd)
		}
	}

	// Ensure that commands are present
	if len(filteredCmds) == 0 {
		return fmt.Errorf("no sql statements in file: %s", fi.Info.Name())
	}

	// Get our checkpoints, if any
	checkpoints, err := m.db.GetMetaCheckpoints(fi.Info.Name())
	if err != nil {
		return errors.Wrap(err, "get checkpoints")
	}
	if len(checkpoints) > 0 {
		m.log.Printf("found %d checkpoints\n", len(checkpoints))
	}

	// Ensure commands weren't deleted from the file after we migrated them
	if len(checkpoints) >= len(filteredCmds) {
		return fmt.Errorf("len(checkpoints) %d >= len(cmds) %d",
			len(checkpoints), len(filteredCmds))
	}

	for i, cmd := range filteredCmds {
		// Confirm the file up to our checkpoint has not changed
		if i < len(checkpoints) {
			r := strings.NewReader(cmd)
			_, checksum, err := computeChecksum(r)
			if err != nil {
				return errors.Wrap(err, "compute checkpoint checksum")
			}
			if checksum != checkpoints[i] {
				return fmt.Errorf(
					"checksum does not equal checkpoint. has %s (cmd %d) changed?",
					fi.Info.Name(), i)
			}
			continue
		}

		// Print the commands we're executing to give progress updates
		// on large migrations
		shortCmd := cmd
		shortCmd = strings.ReplaceAll(shortCmd, "\n", " ")
		shortCmd = spaces.ReplaceAllString(shortCmd, " ")
		if len(shortCmd) >= 78 {
			shortCmd = shortCmd[:74] + "..."
		}
		m.log.Println(">", shortCmd)

		// Execute non-checkpointed commands one by one
		_, err := m.db.Exec(cmd)
		if err != nil {
			m.log.Println("failed on", cmd)
			return fmt.Errorf("%s: %s", fi.Info.Name(), err)
		}

		// Save a checkpoint
		_, checksum, err := computeChecksum(strings.NewReader(cmd))
		if err != nil {
			return errors.Wrap(err, "compute checksum")
		}
		err = m.db.InsertMetaCheckpoint(fi.Info.Name(), cmd, checksum, i)
		if err != nil {
			return errors.Wrap(err, "insert checkpoint")
		}
	}

	// We've successfully finished migrating the file, so we delete the
	// temporary progress in metacheckpoints and save the migration
	if err = m.db.DeleteMetaCheckpoints(); err != nil {
		return errors.Wrap(err, "delete checkpoints")
	}

	_, checksum, err := computeChecksum(bytes.NewReader(byt))
	if err != nil {
		return errors.Wrap(err, "compute file checksum")
	}
	err = m.db.InsertMigration(fi.Info.Name(), string(byt), checksum)
	if err != nil {
		return errors.Wrap(err, "insert migration")
	}
	return nil
}

func (m *Migrate) skip(toFile string) (int, error) {
	// Get just the filename if skip is a directory
	_, toFile = filepath.Split(toFile)

	// Ensure the file exists
	index := -1
	for i, fi := range m.Files {
		if fi.Info.Name() == toFile {
			index = i
			break
		}
	}
	if index == -1 {
		return 0, fmt.Errorf("%s does not exist", toFile)
	}
	for i := 0; i <= index; i++ {
		fi, err := os.Open(m.Files[i].fullpath)
		if err != nil {
			return -1, err
		}
		content, checksum, err := computeChecksum(fi)
		if err != nil {
			fi.Close()
			return -1, err
		}
		name := m.Files[i].Info.Name()
		err = m.db.UpsertMigration(name, content, checksum)
		if err != nil {
			fi.Close()
			return -1, err
		}
		if err = fi.Close(); err != nil {
			return -1, err
		}
	}
	return index, nil
}

func computeChecksum(r io.Reader) (content string, checksum string, err error) {
	h := md5.New()
	byt, err := ioutil.ReadAll(r)
	if err != nil {
		return "", "", errors.Wrap(err, "read all")
	}
	if _, err := io.Copy(h, bytes.NewReader(byt)); err != nil {
		return "", "", err
	}
	return string(byt), fmt.Sprintf("%x", h.Sum(nil)), nil
}

// readDir collects file infos from the migration directory.
func readDir(dir string, dbt DBType) ([]*file, error) {
	files := []*file{}
	tmp, err := ioutil.ReadDir(dir)
	if err != nil {
		return nil, errors.Wrap(err, "read dir")
	}

	// Allow for DB-specific workarounds. For instance, if MySQL and
	// MariaDB are subtly incompatible (and they are, as they name
	// CONSTRAINTS in different ways), then it's possible later migrations
	// will work on one database but not another, even though they should
	// be compatible. There is no easy workaround, especially when you're
	// on an OS with access to one database but not the other.
	//
	// To ease this, we crawl through secondary directories specific to the
	// name of the DB used. If "migrate -t maria-db" then we'll look for
	// the `maria-db` folder and prefer identical migration filenames in
	// that folder over the other one.
	for _, fi := range tmp {
		fullpath := filepath.Join(dir, fi.Name())

		// Skip directories and hidden files
		if fi.IsDir() || strings.HasPrefix(fi.Name(), ".") {
			continue
		}
		// Skip any non-sql files
		if filepath.Ext(fi.Name()) != ".sql" {
			continue
		}
		files = append(files, &file{Info: fi, fullpath: fullpath})
	}
	if len(files) == 0 {
		return nil, errors.New("no sql migration files found (might be the wrong -dir)")
	}

	// Prioritize our specific database over the set in the main migration
	// directory.
	overrideSet, err := getOverrideSet(dir, dbt)
	if err != nil {
		return nil, fmt.Errorf("get override set: %w", err)
	}
	for i, fi := range files {
		if override, exist := overrideSet[fi.Info.Name()]; exist {
			files[i] = override
			fmt.Println("OVERRIDING", override.Info.Name())
		}
	}
	return files, nil
}

func getOverrideSet(dir string, dbt DBType) (map[string]*file, error) {
	tmp, err := ioutil.ReadDir(dir)
	if err != nil {
		return nil, errors.Wrap(err, "read dir")
	}
	overrides := []*file{}
	for _, fi := range tmp {
		fullpath := filepath.Join(dir, fi.Name())
		if !fi.IsDir() || fi.Name() != string(dbt) {
			continue
		}

		// The empty DBType prevents recursive descent into structures
		// like ./mariadb/mariadb/mariadb/...
		overrides, err = readDir(fullpath, DBType(""))
		if err != nil {
			return nil, fmt.Errorf("read dir %s: %w",
				fi.Name(), err)
		}
	}
	overrideSet := make(map[string]*file, len(overrides))
	for _, o := range overrides {
		overrideSet[o.Info.Name()] = o
	}
	return overrideSet, nil
}

// sortFiles by name, ensuring that something like 1.sql, 2.sql, 10.sql is
// ordered correctly.
func sortFiles(files []*file) error {
	var nameErr error
	sort.Slice(files, func(i, j int) bool {
		if nameErr != nil {
			return false
		}
		fiName1 := regexNum.FindString(files[i].Info.Name())
		fiName2 := regexNum.FindString(files[j].Info.Name())
		fiNum1, err := strconv.ParseUint(fiName1, 10, 64)
		if err != nil {
			nameErr = errors.Wrapf(err, "parse uint in file %s",
				files[i].Info.Name())
			return false
		}
		fiNum2, err := strconv.ParseUint(fiName2, 10, 64)
		if err != nil {
			nameErr = errors.Wrapf(err, "parse uint in file %s",
				files[i].Info.Name())
			return false
		}
		if fiNum1 == fiNum2 {
			nameErr = fmt.Errorf("cannot have duplicate timestamp: %d", fiNum1)
			return false
		}
		return fiNum1 < fiNum2
	})
	return nameErr
}

func migrationsFromFiles(m *Migrate) ([]Migration, error) {
	ms := make([]Migration, len(m.Files))
	for i, fi := range m.Files {
		fmt.Println("FULLPATH", fi.fullpath)
		byt, err := ioutil.ReadFile(fi.fullpath)
		if err != nil {
			return nil, errors.Wrap(err, "read file")
		}
		ms[i] = Migration{
			Filename: fi.Info.Name(),
			Content:  string(byt),
			fullpath: fi.fullpath,
		}
	}
	return ms, nil
}
