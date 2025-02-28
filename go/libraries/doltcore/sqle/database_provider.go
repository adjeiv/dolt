// Copyright 2021 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sqle

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/dolthub/go-mysql-server/sql"

	"github.com/dolthub/dolt/go/cmd/dolt/cli"
	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/env"
	"github.com/dolthub/dolt/go/libraries/doltcore/env/actions"
	"github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/libraries/doltcore/schema"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/clusterdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/dfunctions"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/dprocedures"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/dsess"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqlserver"
	"github.com/dolthub/dolt/go/libraries/doltcore/table/editor"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
	"github.com/dolthub/dolt/go/store/datas"
	"github.com/dolthub/dolt/go/store/types"
)

type DoltDatabaseProvider struct {
	// dbLocations maps a database name to its file system root
	dbLocations        map[string]filesys.Filesys
	databases          map[string]dsess.SqlDatabase
	functions          map[string]sql.Function
	externalProcedures sql.ExternalStoredProcedureRegistry
	InitDatabaseHook   InitDatabaseHook
	DropDatabaseHook   DropDatabaseHook
	mu                 *sync.RWMutex

	defaultBranch string
	fs            filesys.Filesys
	remoteDialer  dbfactory.GRPCDialProvider // TODO: why isn't this a method defined on the remote object

	dbFactoryUrl string
	isStandby    *bool
}

var _ sql.DatabaseProvider = (*DoltDatabaseProvider)(nil)
var _ sql.FunctionProvider = (*DoltDatabaseProvider)(nil)
var _ sql.MutableDatabaseProvider = (*DoltDatabaseProvider)(nil)
var _ sql.CollatedDatabaseProvider = (*DoltDatabaseProvider)(nil)
var _ sql.ExternalStoredProcedureProvider = (*DoltDatabaseProvider)(nil)
var _ sql.TableFunctionProvider = (*DoltDatabaseProvider)(nil)
var _ dsess.DoltDatabaseProvider = (*DoltDatabaseProvider)(nil)

// NewDoltDatabaseProvider returns a new provider, initialized without any databases, along with any
// errors that occurred while trying to create the database provider.
func NewDoltDatabaseProvider(defaultBranch string, fs filesys.Filesys) (DoltDatabaseProvider, error) {
	return NewDoltDatabaseProviderWithDatabases(defaultBranch, fs, nil, nil)
}

// NewDoltDatabaseProviderWithDatabase returns a new provider, initialized with one database at the
// specified location, and any error that occurred along the way.
func NewDoltDatabaseProviderWithDatabase(defaultBranch string, fs filesys.Filesys, database dsess.SqlDatabase, dbLocation filesys.Filesys) (DoltDatabaseProvider, error) {
	return NewDoltDatabaseProviderWithDatabases(defaultBranch, fs, []dsess.SqlDatabase{database}, []filesys.Filesys{dbLocation})
}

// NewDoltDatabaseProviderWithDatabases returns a new provider, initialized with the specified databases,
// at the specified locations. For every database specified, there must be a corresponding filesystem
// specified that represents where the database is located. If the number of specified databases is not the
// same as the number of specified locations, an error is returned.
func NewDoltDatabaseProviderWithDatabases(defaultBranch string, fs filesys.Filesys, databases []dsess.SqlDatabase, locations []filesys.Filesys) (DoltDatabaseProvider, error) {
	if len(databases) != len(locations) {
		return DoltDatabaseProvider{}, fmt.Errorf("unable to create DoltDatabaseProvider: "+
			"incorrect number of databases (%d) and database locations (%d) specified", len(databases), len(locations))
	}

	dbs := make(map[string]dsess.SqlDatabase, len(databases))
	for _, db := range databases {
		dbs[strings.ToLower(db.Name())] = db
	}

	dbLocations := make(map[string]filesys.Filesys, len(locations))
	for i, dbLocation := range locations {
		dbLocations[strings.ToLower(databases[i].Name())] = dbLocation
	}

	funcs := make(map[string]sql.Function, len(dfunctions.DoltFunctions))
	for _, fn := range dfunctions.DoltFunctions {
		funcs[strings.ToLower(fn.FunctionName())] = fn
	}

	externalProcedures := sql.NewExternalStoredProcedureRegistry()
	for _, esp := range dprocedures.DoltProcedures {
		externalProcedures.Register(esp)
	}

	// If the specified |fs| is an in mem file system, default to using the InMemDoltDB dbFactoryUrl so that all
	// databases are created with the same file system type.
	dbFactoryUrl := doltdb.LocalDirDoltDB
	if _, ok := fs.(*filesys.InMemFS); ok {
		dbFactoryUrl = doltdb.InMemDoltDB
	}

	return DoltDatabaseProvider{
		dbLocations:        dbLocations,
		databases:          dbs,
		functions:          funcs,
		externalProcedures: externalProcedures,
		mu:                 &sync.RWMutex{},
		fs:                 fs,
		defaultBranch:      defaultBranch,
		dbFactoryUrl:       dbFactoryUrl,
		InitDatabaseHook:   ConfigureReplicationDatabaseHook,
		isStandby:          new(bool),
	}, nil
}

// WithFunctions returns a copy of this provider with the functions given. Any previous functions are removed.
func (p DoltDatabaseProvider) WithFunctions(fns []sql.Function) DoltDatabaseProvider {
	funcs := make(map[string]sql.Function, len(dfunctions.DoltFunctions))
	for _, fn := range fns {
		funcs[strings.ToLower(fn.FunctionName())] = fn
	}

	p.functions = funcs
	return p
}

// WithDbFactoryUrl returns a copy of this provider with the DbFactoryUrl set as provided.
// The URL is used when creating new databases.
// See doltdb.InMemDoltDB, doltdb.LocalDirDoltDB
func (p DoltDatabaseProvider) WithDbFactoryUrl(url string) DoltDatabaseProvider {
	p.dbFactoryUrl = url
	return p
}

// WithRemoteDialer returns a copy of this provider with the dialer provided
func (p DoltDatabaseProvider) WithRemoteDialer(provider dbfactory.GRPCDialProvider) DoltDatabaseProvider {
	p.remoteDialer = provider
	return p
}

func (p DoltDatabaseProvider) FileSystem() filesys.Filesys {
	return p.fs
}

// SetIsStandby sets whether this provider is set to standby |true|. Standbys return every dolt database as a read only
// database. Set back to |false| to get read-write behavior from dolt databases again.
func (p DoltDatabaseProvider) SetIsStandby(standby bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	*p.isStandby = standby
}

// FileSystemForDatabase returns a filesystem, with the working directory set to the root directory
// of the requested database. If the requested database isn't found, a database not found error
// is returned.
func (p DoltDatabaseProvider) FileSystemForDatabase(dbname string) (filesys.Filesys, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	baseName, _ := dsess.SplitRevisionDbName(dbname)

	dbLocation, ok := p.dbLocations[strings.ToLower(baseName)]
	if !ok {
		return nil, sql.ErrDatabaseNotFound.New(dbname)
	}

	return dbLocation, nil
}

// Database implements the sql.DatabaseProvider interface
func (p DoltDatabaseProvider) Database(ctx *sql.Context, name string) (sql.Database, error) {
	database, b, err := p.SessionDatabase(ctx, name)
	if err != nil {
		return nil, err
	}

	if !b {
		return nil, sql.ErrDatabaseNotFound.New(name)
	}

	return database, nil
}

func wrapForStandby(db dsess.SqlDatabase, standby bool) dsess.SqlDatabase {
	if !standby {
		return db
	}
	if _, ok := db.(ReadOnlyDatabase); ok {
		return db
	}
	if db, ok := db.(Database); ok {
		// :-/. Hopefully it's not too sliced.
		return ReadOnlyDatabase{db}
	}
	return db
}

// attemptCloneReplica attempts to clone a database from the configured replication remote URL template, returning an error
// if it cannot be found
// TODO: distinct error for not found v. others
func (p DoltDatabaseProvider) attemptCloneReplica(ctx *sql.Context, dbName string) error {
	// TODO: these need some reworking, they don't make total sense together
	_, readReplicaRemoteName, _ := sql.SystemVariables.GetGlobal(dsess.ReadReplicaRemote)
	if readReplicaRemoteName == "" {
		// not a read replica DB
		return nil
	}

	remoteName := readReplicaRemoteName.(string)

	// TODO: error handling when not set
	_, remoteUrlTemplate, _ := sql.SystemVariables.GetGlobal(dsess.ReplicationRemoteURLTemplate)
	if remoteUrlTemplate == "" {
		return nil
	}

	urlTemplate, ok := remoteUrlTemplate.(string)
	if !ok {
		return nil
	}

	// TODO: url sanitize
	// TODO: SQL identifiers aren't case sensitive, but URLs are, need a plan for this
	remoteUrl := strings.Replace(urlTemplate, dsess.URLTemplateDatabasePlaceholder, dbName, -1)

	// TODO: remote params for AWS, others
	// TODO: this needs to be robust in the face of the DB not having the default branch
	// TODO: this treats every database not found error as a clone error, need to tighten
	err := p.CloneDatabaseFromRemote(ctx, dbName, p.defaultBranch, remoteName, remoteUrl, nil)
	if err != nil {
		return err
	}

	return nil
}

func (p DoltDatabaseProvider) HasDatabase(ctx *sql.Context, name string) bool {
	_, err := p.Database(ctx, name)
	if err != nil && !sql.ErrDatabaseNotFound.Is(err) {
		ctx.GetLogger().Warnf("Error getting database %s: %s", name, err.Error())
	}
	return err == nil
}

func (p DoltDatabaseProvider) AllDatabases(ctx *sql.Context) (all []sql.Database) {
	currentDb := ctx.GetCurrentDatabase()
	_, currRev := dsess.SplitRevisionDbName(currentDb)

	p.mu.RLock()
	showBranches, _ := dsess.GetBooleanSystemVar(ctx, dsess.ShowBranchDatabases)

	all = make([]sql.Database, 0, len(p.databases))
	for _, db := range p.databases {
		all = append(all, db)

		if showBranches && db.Name() != clusterdb.DoltClusterDbName {
			revisionDbs, err := p.allRevisionDbs(ctx, db)
			if err != nil {
				// TODO: this interface is wrong, needs to return errors
				ctx.GetLogger().Warnf("error fetching revision databases: %s", err.Error())
				continue
			}
			all = append(all, revisionDbs...)
		}
	}
	p.mu.RUnlock()

	// If there's a revision database in use, include it in the list (but don't double-count)
	if currRev != "" && !showBranches {
		rdb, ok, err := p.databaseForRevision(ctx, currentDb, currentDb)
		if err != nil || !ok {
			// TODO: this interface is wrong, needs to return errors
			ctx.GetLogger().Warnf("error fetching revision databases: %s", err.Error())
		} else {
			all = append(all, rdb)
		}
	}

	// Because we store databases in a map, sort to get a consistent ordering
	sort.Slice(all, func(i, j int) bool {
		return strings.ToLower(all[i].Name()) < strings.ToLower(all[j].Name())
	})

	return all
}

// DoltDatabases implements the dsess.DoltDatabaseProvider interface
func (p DoltDatabaseProvider) DoltDatabases() []dsess.SqlDatabase {
	p.mu.RLock()
	defer p.mu.RUnlock()

	dbs := make([]dsess.SqlDatabase, len(p.databases))
	i := 0
	for _, db := range p.databases {
		dbs[i] = db
		i++
	}

	sort.Slice(dbs, func(i, j int) bool {
		return strings.ToLower(dbs[i].Name()) < strings.ToLower(dbs[j].Name())
	})

	return dbs
}

// allRevisionDbs returns all revision dbs for the database given
func (p DoltDatabaseProvider) allRevisionDbs(ctx *sql.Context, db dsess.SqlDatabase) ([]sql.Database, error) {
	branches, err := db.DbData().Ddb.GetBranches(ctx)
	if err != nil {
		return nil, err
	}

	revDbs := make([]sql.Database, len(branches))
	for i, branch := range branches {
		revisionQualifiedName := fmt.Sprintf("%s/%s", db.Name(), branch.GetPath())
		revDb, ok, err := p.databaseForRevision(ctx, revisionQualifiedName, revisionQualifiedName)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("cannot get revision database for %s/%s", db.Name(), branch.GetPath())
		}
		revDbs[i] = revDb
	}

	return revDbs, nil
}

func (p DoltDatabaseProvider) GetRemoteDB(ctx context.Context, format *types.NomsBinFormat, r env.Remote, withCaching bool) (*doltdb.DoltDB, error) {
	if withCaching {
		return r.GetRemoteDB(ctx, format, p.remoteDialer)
	}
	return r.GetRemoteDBWithoutCaching(ctx, format, p.remoteDialer)
}

func (p DoltDatabaseProvider) CreateDatabase(ctx *sql.Context, name string) error {
	return p.CreateCollatedDatabase(ctx, name, sql.Collation_Default)
}

func (p DoltDatabaseProvider) CreateCollatedDatabase(ctx *sql.Context, name string, collation sql.CollationID) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	exists, isDir := p.fs.Exists(name)
	if exists && isDir {
		return sql.ErrDatabaseExists.New(name)
	} else if exists {
		return fmt.Errorf("Cannot create DB, file exists at %s", name)
	}

	err := p.fs.MkDirs(name)
	if err != nil {
		return err
	}

	newFs, err := p.fs.WithWorkingDir(name)
	if err != nil {
		return err
	}

	// TODO: fill in version appropriately
	sess := dsess.DSessFromSess(ctx.Session)
	newEnv := env.Load(ctx, env.GetCurrentUserHomeDir, newFs, p.dbFactoryUrl, "TODO")

	newDbStorageFormat := types.Format_Default
	err = newEnv.InitRepo(ctx, newDbStorageFormat, sess.Username(), sess.Email(), p.defaultBranch)
	if err != nil {
		return err
	}

	// Set the collation
	if collation != sql.Collation_Default {
		workingRoot, err := newEnv.WorkingRoot(ctx)
		if err != nil {
			return err
		}
		newRoot, err := workingRoot.SetCollation(ctx, schema.Collation(collation))
		if err != nil {
			return err
		}
		// As this is a newly created database, we set both the working and staged roots to the same root value
		if err = newEnv.UpdateWorkingRoot(ctx, newRoot); err != nil {
			return err
		}
		if err = newEnv.UpdateStagedRoot(ctx, newRoot); err != nil {
			return err
		}
	}

	// If we're running in a sql-server context, ensure the new database is locked so that it can't
	// be edited from the CLI. We can't rely on looking for an existing lock file, since this could
	// be the first db creation if sql-server was started from a bare directory.
	_, lckDeets := sqlserver.GetRunningServer()
	if lckDeets != nil {
		err = newEnv.Lock(lckDeets)
		if err != nil {
			ctx.GetLogger().Warnf("Failed to lock newly created database: %s", err.Error())
		}
	}

	fkChecks, err := ctx.GetSessionVariable(ctx, "foreign_key_checks")
	if err != nil {
		return err
	}

	opts := editor.Options{
		Deaf: newEnv.DbEaFactory(),
		// TODO: this doesn't seem right, why is this getting set in the constructor to the DB
		ForeignKeyChecksDisabled: fkChecks.(int8) == 0,
	}

	db, err := NewDatabase(ctx, name, newEnv.DbData(), opts)
	if err != nil {
		return err
	}

	// If we have an initialization hook, invoke it.  By default, this will
	// be ConfigureReplicationDatabaseHook, which will setup replication
	// for the new database if a remote url template is set.
	err = p.InitDatabaseHook(ctx, p, name, newEnv)
	if err != nil {
		return err
	}

	formattedName := formatDbMapKeyName(db.Name())
	p.databases[formattedName] = db
	p.dbLocations[formattedName] = newEnv.FS

	return nil
}

type InitDatabaseHook func(ctx *sql.Context, pro DoltDatabaseProvider, name string, env *env.DoltEnv) error
type DropDatabaseHook func(name string)

// ConfigureReplicationDatabaseHook sets up replication for a newly created database as necessary
// TODO: consider the replication heads / all heads setting
func ConfigureReplicationDatabaseHook(ctx *sql.Context, p DoltDatabaseProvider, name string, newEnv *env.DoltEnv) error {
	_, replicationRemoteName, _ := sql.SystemVariables.GetGlobal(dsess.ReplicateToRemote)
	if replicationRemoteName == "" {
		return nil
	}

	remoteName, ok := replicationRemoteName.(string)
	if !ok {
		return nil
	}

	_, remoteUrlTemplate, _ := sql.SystemVariables.GetGlobal(dsess.ReplicationRemoteURLTemplate)
	if remoteUrlTemplate == "" {
		return nil
	}

	urlTemplate, ok := remoteUrlTemplate.(string)
	if !ok {
		return nil
	}

	// TODO: url sanitize name
	remoteUrl := strings.Replace(urlTemplate, dsess.URLTemplateDatabasePlaceholder, name, -1)

	// TODO: params for AWS, others that need them
	r := env.NewRemote(remoteName, remoteUrl, nil)
	err := r.Prepare(ctx, newEnv.DoltDB.Format(), p.remoteDialer)
	if err != nil {
		return err
	}

	err = newEnv.AddRemote(r)
	if err != env.ErrRemoteAlreadyExists && err != nil {
		return err
	}

	// TODO: get background threads from the engine
	commitHooks, err := GetCommitHooks(ctx, sql.NewBackgroundThreads(), newEnv, cli.CliErr)
	if err != nil {
		return err
	}

	newEnv.DoltDB.SetCommitHooks(ctx, commitHooks)

	// After setting hooks on the newly created DB, we need to do the first push manually
	branchRef := ref.NewBranchRef(p.defaultBranch)
	return newEnv.DoltDB.ExecuteCommitHooks(ctx, branchRef.String())
}

// CloneDatabaseFromRemote implements DoltDatabaseProvider interface
func (p DoltDatabaseProvider) CloneDatabaseFromRemote(
	ctx *sql.Context,
	dbName, branch, remoteName, remoteUrl string,
	remoteParams map[string]string,
) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	exists, isDir := p.fs.Exists(dbName)
	if exists && isDir {
		return sql.ErrDatabaseExists.New(dbName)
	} else if exists {
		return fmt.Errorf("cannot create DB, file exists at %s", dbName)
	}

	dEnv, err := p.cloneDatabaseFromRemote(ctx, dbName, remoteName, branch, remoteUrl, remoteParams)
	if err != nil {
		// Make a best effort to clean up any artifacts on disk from a failed clone
		// before we return the error
		exists, _ := p.fs.Exists(dbName)
		if exists {
			deleteErr := p.fs.Delete(dbName, true)
			if deleteErr != nil {
				err = fmt.Errorf("%s: unable to clean up failed clone in directory '%s'", err.Error(), dbName)
			}
		}
		return err
	}

	return ConfigureReplicationDatabaseHook(ctx, p, dbName, dEnv)
}

// cloneDatabaseFromRemote encapsulates the inner logic for cloning a database so that if any error
// is returned by this function, the caller can capture the error and safely clean up the failed
// clone directory before returning the error to the user. This function should not be used directly;
// use CloneDatabaseFromRemote instead.
func (p DoltDatabaseProvider) cloneDatabaseFromRemote(
	ctx *sql.Context,
	dbName, remoteName, branch, remoteUrl string,
	remoteParams map[string]string,
) (*env.DoltEnv, error) {
	if p.remoteDialer == nil {
		return nil, fmt.Errorf("unable to clone remote database; no remote dialer configured")
	}

	// TODO: params for AWS, others that need them
	r := env.NewRemote(remoteName, remoteUrl, nil)
	srcDB, err := r.GetRemoteDB(ctx, types.Format_Default, p.remoteDialer)
	if err != nil {
		return nil, err
	}

	dEnv, err := actions.EnvForClone(ctx, srcDB.ValueReadWriter().Format(), r, dbName, p.fs, "VERSION", env.GetCurrentUserHomeDir)
	if err != nil {
		return nil, err
	}

	err = actions.CloneRemote(ctx, srcDB, remoteName, branch, dEnv)
	if err != nil {
		return nil, err
	}

	err = dEnv.RepoStateWriter().UpdateBranch(dEnv.RepoState.CWBHeadRef().GetPath(), env.BranchConfig{
		Merge:  dEnv.RepoState.Head,
		Remote: remoteName,
	})

	fkChecks, err := ctx.GetSessionVariable(ctx, "foreign_key_checks")
	if err != nil {
		return nil, err
	}

	opts := editor.Options{
		Deaf: dEnv.DbEaFactory(),
		// TODO: this doesn't seem right, why is this getting set in the constructor to the DB
		ForeignKeyChecksDisabled: fkChecks.(int8) == 0,
	}

	db, err := NewDatabase(ctx, dbName, dEnv.DbData(), opts)
	if err != nil {
		return nil, err
	}

	// If we have an initialization hook, invoke it.  By default, this will
	// be ConfigureReplicationDatabaseHook, which will setup replication
	// for the new database if a remote url template is set.
	err = p.InitDatabaseHook(ctx, p, dbName, dEnv)
	if err != nil {
		return nil, err
	}

	p.databases[formatDbMapKeyName(db.Name())] = db

	return dEnv, nil
}

// DropDatabase implements the sql.MutableDatabaseProvider interface
func (p DoltDatabaseProvider) DropDatabase(ctx *sql.Context, name string) error {
	_, revision := dsess.SplitRevisionDbName(name)
	if revision != "" {
		return fmt.Errorf("unable to drop revision database: %s", name)
	}

	// get the case-sensitive name for case-sensitive file systems
	// TODO: there are still cases (not server-first) where we rename databases because the directory name would need
	//  quoting if used as a database name, and that breaks here. We either need the database name to match the directory
	//  name in all cases, or else keep a mapping from database name to directory on disk.
	p.mu.Lock()
	defer p.mu.Unlock()

	dbKey := formatDbMapKeyName(name)
	db := p.databases[dbKey]

	ddb := db.(Database).ddb
	err := ddb.Close()
	if err != nil {
		return err
	}

	// get location of database that's being dropped
	dbLoc := p.dbLocations[dbKey]
	if dbLoc == nil {
		return sql.ErrDatabaseNotFound.New(db.Name())
	}

	dropDbLoc, err := dbLoc.Abs("")
	if err != nil {
		return err
	}

	// If this database is re-created, we don't want to return any cached results.
	err = dbfactory.DeleteFromSingletonCache(dropDbLoc + "/.dolt/noms")
	if err != nil {
		return err
	}

	if p.DropDatabaseHook != nil {
		// For symmetry with InitDatabaseHook and the names we see in
		// MultiEnv initialization, we use `name` here, not `dbKey`.
		p.DropDatabaseHook(name)
	}

	rootDbLoc, err := p.fs.Abs("")
	if err != nil {
		return err
	}
	dirToDelete := ""
	// if the database is in the directory itself, we remove '.dolt' directory rather than
	// the whole directory itself because it can have other databases that are nested.
	if rootDbLoc == dropDbLoc {
		doltDirExists, _ := p.fs.Exists(dbfactory.DoltDir)
		if !doltDirExists {
			return sql.ErrDatabaseNotFound.New(db.Name())
		}
		dirToDelete = dbfactory.DoltDir
	} else {
		exists, isDir := p.fs.Exists(dropDbLoc)
		// Get the DB's directory
		if !exists {
			// engine should already protect against this
			return sql.ErrDatabaseNotFound.New(db.Name())
		} else if !isDir {
			return fmt.Errorf("unexpected error: %s exists but is not a directory", dbKey)
		}
		dirToDelete = dropDbLoc
	}

	err = p.fs.Delete(dirToDelete, true)
	if err != nil {
		return err
	}

	// We not only have to delete this database, but any derivative ones that we've stored as a result of USE or
	// connection strings
	derivativeNamePrefix := strings.ToLower(dbKey + dsess.DbRevisionDelimiter)
	for dbName := range p.databases {
		if strings.HasPrefix(strings.ToLower(dbName), derivativeNamePrefix) {
			delete(p.databases, dbName)
		}
	}

	delete(p.databases, dbKey)

	return p.invalidateDbStateInAllSessions(ctx, name)
}

// invalidateDbStateInAllSessions removes the db state for this database from every session. This is necessary when a
// database is dropped, so that other sessions don't use stale db state.
func (p DoltDatabaseProvider) invalidateDbStateInAllSessions(ctx *sql.Context, name string) error {
	// Remove the db state from the current session
	err := dsess.DSessFromSess(ctx.Session).RemoveDbState(ctx, name)
	if err != nil {
		return err
	}

	// If we have a running server, remove it from other sessions as well
	runningServer, _ := sqlserver.GetRunningServer()
	if runningServer != nil {
		sessionManager := runningServer.SessionManager()
		err := sessionManager.Iter(func(session sql.Session) (bool, error) {
			dsess, ok := session.(*dsess.DoltSession)
			if !ok {
				return false, fmt.Errorf("unexpected session type: %T", session)
			}

			// We need to invalidate this database state for EVERY session, even if other sessions aren't actively
			// using this database, since they could still reference it with a db-qualified table name.
			err := dsess.RemoveDbState(ctx, name)
			if err != nil {
				return true, err
			}
			return false, nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func (p DoltDatabaseProvider) databaseForRevision(ctx *sql.Context, revisionQualifiedName string, requestedName string) (dsess.SqlDatabase, bool, error) {
	if !strings.Contains(revisionQualifiedName, dsess.DbRevisionDelimiter) {
		return nil, false, nil
	}

	parts := strings.SplitN(revisionQualifiedName, dsess.DbRevisionDelimiter, 2)
	baseName, rev := parts[0], parts[1]

	// Look in the session cache for this DB before doing any IO to figure out what's being asked for
	sess := dsess.DSessFromSess(ctx.Session)
	dbCache := sess.DatabaseCache(ctx)
	db, ok := dbCache.GetCachedRevisionDb(revisionQualifiedName, requestedName)
	if ok {
		return db, true, nil
	}

	p.mu.RLock()
	srcDb, ok := p.databases[formatDbMapKeyName(baseName)]
	p.mu.RUnlock()
	if !ok {
		return nil, false, nil
	}

	dbType, resolvedRevSpec, err := revisionDbType(ctx, srcDb, rev)
	if err != nil {
		return nil, false, err
	}

	switch dbType {
	case dsess.RevisionTypeBranch:
		// fetch the upstream head if this is a replicated db
		replicaDb, ok := srcDb.(ReadReplicaDatabase)
		if ok && replicaDb.ValidReplicaState(ctx) {
			// TODO move this out of analysis phase, should only happen at read time, when the transaction begins (like is
			//  the case with a branch that already exists locally)
			err := p.ensureReplicaHeadExists(ctx, resolvedRevSpec, replicaDb)
			if err != nil {
				return nil, false, err
			}
		}

		db, err := revisionDbForBranch(ctx, srcDb, resolvedRevSpec, requestedName)
		// preserve original user case in the case of not found
		if sql.ErrDatabaseNotFound.Is(err) {
			return nil, false, sql.ErrDatabaseNotFound.New(revisionQualifiedName)
		} else if err != nil {
			return nil, false, err
		}

		dbCache.CacheRevisionDb(db)
		return db, true, nil
	case dsess.RevisionTypeTag:
		// TODO: this should be an interface, not a struct
		replicaDb, ok := srcDb.(ReadReplicaDatabase)

		if ok {
			srcDb = replicaDb.Database
		}

		srcDb, ok = srcDb.(Database)
		if !ok {
			return nil, false, nil
		}

		db, err := revisionDbForTag(ctx, srcDb.(Database), resolvedRevSpec, requestedName)
		if err != nil {
			return nil, false, err
		}

		dbCache.CacheRevisionDb(db)
		return db, true, nil
	case dsess.RevisionTypeCommit:
		// TODO: this should be an interface, not a struct
		replicaDb, ok := srcDb.(ReadReplicaDatabase)
		if ok {
			srcDb = replicaDb.Database
		}

		srcDb, ok = srcDb.(Database)
		if !ok {
			return nil, false, nil
		}

		db, err := revisionDbForCommit(ctx, srcDb.(Database), rev, requestedName)
		if err != nil {
			return nil, false, err
		}

		dbCache.CacheRevisionDb(db)
		return db, true, nil
	case dsess.RevisionTypeNone:
		// Returning an error with the fully qualified db name here is our only opportunity to do so in some cases (such
		// as when a branch is deleted by another client)
		return nil, false, sql.ErrDatabaseNotFound.New(revisionQualifiedName)
	default:
		return nil, false, fmt.Errorf("unrecognized revision type for revision spec %s", rev)
	}
}

// revisionDbType returns the type of revision spec given for the database given, and the resolved revision spec
func revisionDbType(ctx *sql.Context, srcDb dsess.SqlDatabase, revSpec string) (revType dsess.RevisionType, resolvedRevSpec string, err error) {
	resolvedRevSpec, err = resolveAncestorSpec(ctx, revSpec, srcDb.DbData().Ddb)
	if err != nil {
		return dsess.RevisionTypeNone, "", err
	}

	caseSensitiveBranchName, isBranch, err := isBranch(ctx, srcDb, resolvedRevSpec)
	if err != nil {
		return dsess.RevisionTypeNone, "", err
	}

	if isBranch {
		return dsess.RevisionTypeBranch, caseSensitiveBranchName, nil
	}

	isTag, err := isTag(ctx, srcDb, resolvedRevSpec)
	if err != nil {
		return dsess.RevisionTypeNone, "", err
	}

	if isTag {
		return dsess.RevisionTypeTag, resolvedRevSpec, nil
	}

	if doltdb.IsValidCommitHash(resolvedRevSpec) {
		// IsValidCommitHash just checks a regex, we need to see if the commit actually exists
		valid, err := isValidCommitHash(ctx, srcDb, resolvedRevSpec)
		if err != nil {
			return 0, "", err
		}

		if valid {
			return dsess.RevisionTypeCommit, resolvedRevSpec, nil
		}
	}

	return dsess.RevisionTypeNone, "", nil
}

func isValidCommitHash(ctx *sql.Context, db dsess.SqlDatabase, commitHash string) (bool, error) {
	cs, err := doltdb.NewCommitSpec(commitHash)
	if err != nil {
		return false, err
	}

	for _, ddb := range db.DoltDatabases() {
		_, err = ddb.Resolve(ctx, cs, nil)
		if errors.Is(err, datas.ErrCommitNotFound) {
			continue
		} else if err != nil {
			return false, err
		}

		return true, nil
	}

	return false, nil
}

func initialDbState(ctx context.Context, db dsess.SqlDatabase, branch string) (dsess.InitialDbState, error) {
	rsr := db.DbData().Rsr
	ddb := db.DbData().Ddb

	var r ref.DoltRef
	if len(branch) > 0 {
		r = ref.NewBranchRef(branch)
	} else {
		var err error
		r, err = rsr.CWBHeadRef()
		if err != nil {
			return dsess.InitialDbState{}, err
		}
	}

	var retainedErr error

	headCommit, err := ddb.ResolveCommitRef(ctx, r)
	if err == doltdb.ErrBranchNotFound {
		retainedErr = err
		err = nil
	}
	if err != nil {
		return dsess.InitialDbState{}, err
	}

	var ws *doltdb.WorkingSet
	if retainedErr == nil {
		workingSetRef, err := ref.WorkingSetRefForHead(r)
		if err != nil {
			return dsess.InitialDbState{}, err
		}

		ws, err = db.DbData().Ddb.ResolveWorkingSet(ctx, workingSetRef)
		if err != nil {
			return dsess.InitialDbState{}, err
		}
	}

	remotes, err := rsr.GetRemotes()
	if err != nil {
		return dsess.InitialDbState{}, err
	}

	backups, err := rsr.GetBackups()
	if err != nil {
		return dsess.InitialDbState{}, err
	}

	branches, err := rsr.GetBranches()
	if err != nil {
		return dsess.InitialDbState{}, err
	}

	return dsess.InitialDbState{
		Db:         db,
		HeadCommit: headCommit,
		WorkingSet: ws,
		DbData:     db.DbData(),
		Remotes:    remotes,
		Branches:   branches,
		Backups:    backups,
		Err:        retainedErr,
	}, nil
}

func initialStateForRevisionDb(ctx *sql.Context, db dsess.SqlDatabase) (dsess.InitialDbState, error) {
	switch db.RevisionType() {
	case dsess.RevisionTypeBranch:
		init, err := initialStateForBranchDb(ctx, db)
		// preserve original user case in the case of not found
		if sql.ErrDatabaseNotFound.Is(err) {
			return dsess.InitialDbState{}, sql.ErrDatabaseNotFound.New(db.Name())
		} else if err != nil {
			return dsess.InitialDbState{}, err
		}

		return init, nil
	case dsess.RevisionTypeTag:
		// TODO: this should be an interface, not a struct
		replicaDb, ok := db.(ReadReplicaDatabase)

		if ok {
			db = replicaDb.Database
		}

		db, ok = db.(ReadOnlyDatabase)
		if !ok {
			return dsess.InitialDbState{}, fmt.Errorf("expected a ReadOnlyDatabase, got %T", db)
		}

		init, err := initialStateForTagDb(ctx, db.(ReadOnlyDatabase))
		if err != nil {
			return dsess.InitialDbState{}, err
		}

		return init, nil
	case dsess.RevisionTypeCommit:
		// TODO: this should be an interface, not a struct
		replicaDb, ok := db.(ReadReplicaDatabase)
		if ok {
			db = replicaDb.Database
		}

		db, ok = db.(ReadOnlyDatabase)
		if !ok {
			return dsess.InitialDbState{}, fmt.Errorf("expected a ReadOnlyDatabase, got %T", db)
		}

		init, err := initialStateForCommit(ctx, db.(ReadOnlyDatabase))
		if err != nil {
			return dsess.InitialDbState{}, err
		}
		return init, nil
	default:
		return dsess.InitialDbState{}, fmt.Errorf("unrecognized revision type for revision spec %s: %v", db.Revision(), db.RevisionType())
	}
}

// databaseForClone returns a newly cloned database if read replication is enabled and a remote DB exists, or an error
// otherwise
func (p DoltDatabaseProvider) databaseForClone(ctx *sql.Context, revDB string) (dsess.SqlDatabase, error) {
	if !readReplicationActive(ctx) {
		return nil, nil
	}

	var dbName string
	if strings.Contains(revDB, dsess.DbRevisionDelimiter) {
		parts := strings.SplitN(revDB, dsess.DbRevisionDelimiter, 2)
		dbName = parts[0]
	} else {
		dbName = revDB
	}

	err := p.attemptCloneReplica(ctx, dbName)
	if err != nil {
		ctx.GetLogger().Warnf("couldn't clone database %s: %s", dbName, err.Error())
		return nil, nil
	}

	// This database needs to be added to the transaction
	// TODO: we should probably do all this pulling on transaction start, rather than pulling automatically when the
	//  DB is first referenced
	tx, ok := ctx.GetTransaction().(*dsess.DoltTransaction)
	if ok {
		db := p.databases[dbName]
		err = tx.AddDb(ctx, db)
		if err != nil {
			return nil, err
		}
	}

	// now that the database has been cloned, retry the Database call
	database, err := p.Database(ctx, revDB)
	return database.(dsess.SqlDatabase), err
}

// TODO: figure out the right contract: which variables must be set? What happens if they aren't all set?
func readReplicationActive(ctx *sql.Context) bool {
	_, readReplicaRemoteName, _ := sql.SystemVariables.GetGlobal(dsess.ReadReplicaRemote)
	if readReplicaRemoteName == "" {
		return false
	}

	_, remoteUrlTemplate, _ := sql.SystemVariables.GetGlobal(dsess.ReplicationRemoteURLTemplate)
	if remoteUrlTemplate == "" {
		return false
	}

	return true
}

// resolveAncestorSpec resolves the specified revSpec to a specific commit hash if it contains an ancestor reference
// such as ~ or ^. If no ancestor reference is present, the specified revSpec is returned as is. If any unexpected
// problems are encountered, an error is returned.
func resolveAncestorSpec(ctx *sql.Context, revSpec string, ddb *doltdb.DoltDB) (string, error) {
	refname, ancestorSpec, err := doltdb.SplitAncestorSpec(revSpec)
	if err != nil {
		return "", err
	}
	if ancestorSpec == nil || ancestorSpec.SpecStr == "" {
		return revSpec, nil
	}

	ref, err := ddb.GetRefByNameInsensitive(ctx, refname)
	if err != nil {
		return "", err
	}

	cm, err := ddb.ResolveCommitRef(ctx, ref)
	if err != nil {
		return "", err
	}

	cm, err = cm.GetAncestor(ctx, ancestorSpec)
	if err != nil {
		return "", err
	}

	hash, err := cm.HashOf()
	if err != nil {
		return "", err
	}

	return hash.String(), nil
}

// BaseDatabase returns the base database for the specified database name. Meant for informational purposes when
// managing the session initialization only. Use SessionDatabase for normal database retrieval.
func (p DoltDatabaseProvider) BaseDatabase(ctx *sql.Context, name string) (dsess.SqlDatabase, bool) {
	baseName := name
	isRevisionDbName := strings.Contains(name, dsess.DbRevisionDelimiter)

	if isRevisionDbName {
		parts := strings.SplitN(name, dsess.DbRevisionDelimiter, 2)
		baseName = parts[0]
	}

	var ok bool
	p.mu.RLock()
	db, ok := p.databases[strings.ToLower(baseName)]
	p.mu.RUnlock()

	return db, ok
}

// SessionDatabase implements dsess.SessionDatabaseProvider
func (p DoltDatabaseProvider) SessionDatabase(ctx *sql.Context, name string) (dsess.SqlDatabase, bool, error) {
	baseName := name
	isRevisionDbName := strings.Contains(name, dsess.DbRevisionDelimiter)

	if isRevisionDbName {
		// TODO: formalize and enforce this rule (can't allow DBs with / in the name)
		// TODO: some connectors will take issue with the /, we need other mechanisms to support them
		parts := strings.SplitN(name, dsess.DbRevisionDelimiter, 2)
		baseName = parts[0]
	}

	var ok bool
	p.mu.RLock()
	db, ok := p.databases[strings.ToLower(baseName)]
	standby := *p.isStandby
	p.mu.RUnlock()

	// If the database doesn't exist and this is a read replica, attempt to clone it from the remote
	if !ok {
		var err error
		db, err = p.databaseForClone(ctx, strings.ToLower(baseName))

		if err != nil {
			return nil, false, err
		}

		if db == nil {
			return nil, false, nil
		}
	}

	// Some DB implementations don't support addressing by versioned names, so return directly if we have one of those
	if !db.Versioned() {
		return wrapForStandby(db, standby), true, nil
	}

	// Convert to a revision database before returning. If we got a non-qualified name, convert it to a qualified name
	// using the session's current head
	revisionQualifiedName := name
	usingDefaultBranch := false
	head := ""
	sess := dsess.DSessFromSess(ctx.Session)
	if !isRevisionDbName {
		var err error
		head, ok, err = sess.CurrentHead(ctx, baseName)
		if err != nil {
			return nil, false, err
		}

		// A newly created session may not have any info on current head stored yet, in which case we get the default
		// branch for the db itself instead.
		if !ok {
			usingDefaultBranch = true

			head, err = dsess.DefaultHead(baseName, db)
			if err != nil {
				return nil, false, err
			}
		}

		revisionQualifiedName = baseName + dsess.DbRevisionDelimiter + head
	}

	db, ok, err := p.databaseForRevision(ctx, revisionQualifiedName, name)
	if err != nil {
		if sql.ErrDatabaseNotFound.Is(err) && usingDefaultBranch {
			// We can return a better error message here in some cases
			// TODO: this better error message doesn't always get returned to clients because the code path is doesn't
			//  return an error, only a boolean result (HasDB)
			return nil, false, fmt.Errorf("cannot resolve default branch head for database '%s': '%s'", baseName, head)
		} else {
			return nil, false, err
		}
	}

	if !ok {
		return nil, false, nil
	}

	return wrapForStandby(db, standby), true, nil
}

// Function implements the FunctionProvider interface
func (p DoltDatabaseProvider) Function(_ *sql.Context, name string) (sql.Function, error) {
	fn, ok := p.functions[strings.ToLower(name)]
	if !ok {
		return nil, sql.ErrFunctionNotFound.New(name)
	}
	return fn, nil
}

func (p DoltDatabaseProvider) Register(d sql.ExternalStoredProcedureDetails) {
	p.externalProcedures.Register(d)
}

// ExternalStoredProcedure implements the sql.ExternalStoredProcedureProvider interface
func (p DoltDatabaseProvider) ExternalStoredProcedure(_ *sql.Context, name string, numOfParams int) (*sql.ExternalStoredProcedureDetails, error) {
	return p.externalProcedures.LookupByNameAndParamCount(name, numOfParams)
}

// ExternalStoredProcedures implements the sql.ExternalStoredProcedureProvider interface
func (p DoltDatabaseProvider) ExternalStoredProcedures(_ *sql.Context, name string) ([]sql.ExternalStoredProcedureDetails, error) {
	return p.externalProcedures.LookupByName(name)
}

// TableFunction implements the sql.TableFunctionProvider interface
func (p DoltDatabaseProvider) TableFunction(_ *sql.Context, name string) (sql.TableFunction, error) {
	// TODO: Clean this up and store table functions in a map, similar to regular functions.
	switch strings.ToLower(name) {
	case "dolt_diff":
		dtf := &DiffTableFunction{}
		return dtf, nil
	case "dolt_diff_stat":
		dtf := &DiffStatTableFunction{}
		return dtf, nil
	case "dolt_diff_summary":
		dtf := &DiffSummaryTableFunction{}
		return dtf, nil
	case "dolt_log":
		dtf := &LogTableFunction{}
		return dtf, nil
	case "dolt_patch":
		dtf := &PatchTableFunction{}
		return dtf, nil
	case "dolt_schema_diff":
		dtf := &SchemaDiffTableFunction{}
		return dtf, nil
	case "dolt_query_diff":
		dtf := &QueryDiffTableFunction{}
		return dtf, nil
	}

	return nil, sql.ErrTableFunctionNotFound.New(name)
}

// ensureReplicaHeadExists tries to pull the latest version of a remote branch. Will fail if the branch
// does not exist on the ReadReplicaDatabase's remote.
func (p DoltDatabaseProvider) ensureReplicaHeadExists(ctx *sql.Context, branch string, db ReadReplicaDatabase) error {
	return db.CreateLocalBranchFromRemote(ctx, ref.NewBranchRef(branch))
}

// isBranch returns whether a branch with the given name is in scope for the database given
func isBranch(ctx context.Context, db dsess.SqlDatabase, branchName string) (string, bool, error) {
	ddbs := db.DoltDatabases()

	brName, branchExists, err := isLocalBranch(ctx, ddbs, branchName)
	if err != nil {
		return "", false, err
	}
	if branchExists {
		return brName, true, nil
	}

	brName, branchExists, err = isRemoteBranch(ctx, ddbs, branchName)
	if err != nil {
		return "", false, err
	}
	if branchExists {
		return brName, true, nil
	}

	return "", false, nil
}

func isLocalBranch(ctx context.Context, ddbs []*doltdb.DoltDB, branchName string) (string, bool, error) {
	for _, ddb := range ddbs {
		brName, branchExists, err := ddb.HasBranch(ctx, branchName)
		if err != nil {
			return "", false, err
		}

		if branchExists {
			return brName, true, nil
		}
	}

	return "", false, nil
}

// isRemoteBranch returns whether the given branch name is a remote branch on any of the databases provided.
func isRemoteBranch(ctx context.Context, ddbs []*doltdb.DoltDB, branchName string) (string, bool, error) {
	for _, ddb := range ddbs {
		bn, branchExists, _, err := ddb.HasRemoteTrackingBranch(ctx, branchName)
		if err != nil {
			return "", false, err
		}

		if branchExists {
			return bn, true, nil
		}
	}

	return "", false, nil
}

// isTag returns whether a tag with the given name is in scope for the database given
func isTag(ctx context.Context, db dsess.SqlDatabase, tagName string) (bool, error) {
	ddbs := db.DoltDatabases()

	for _, ddb := range ddbs {
		tagExists, err := ddb.HasTag(ctx, tagName)
		if err != nil {
			return false, err
		}

		if tagExists {
			return true, nil
		}
	}

	return false, nil
}

// revisionDbForBranch returns a new database that is tied to the branch named by revSpec
func revisionDbForBranch(ctx context.Context, srcDb dsess.SqlDatabase, revSpec string, requestedName string) (dsess.SqlDatabase, error) {
	static := staticRepoState{
		branch:          ref.NewBranchRef(revSpec),
		RepoStateWriter: srcDb.DbData().Rsw,
		RepoStateReader: srcDb.DbData().Rsr,
	}

	return srcDb.WithBranchRevision(requestedName, dsess.SessionDatabaseBranchSpec{
		RepoState: static,
		Branch:    revSpec,
	})
}

func initialStateForBranchDb(ctx *sql.Context, srcDb dsess.SqlDatabase) (dsess.InitialDbState, error) {
	revSpec := srcDb.Revision()

	// TODO: this may be a disabled transaction, need to kill those
	rootHash, err := dsess.TransactionRoot(ctx, srcDb)
	if err != nil {
		return dsess.InitialDbState{}, err
	}

	branch := ref.NewBranchRef(revSpec)
	cm, err := srcDb.DbData().Ddb.ResolveCommitRefAtRoot(ctx, branch, rootHash)
	if err != nil {
		return dsess.InitialDbState{}, err
	}

	wsRef, err := ref.WorkingSetRefForHead(branch)
	if err != nil {
		return dsess.InitialDbState{}, err
	}

	ws, err := srcDb.DbData().Ddb.ResolveWorkingSetAtRoot(ctx, wsRef, rootHash)
	if err != nil {
		return dsess.InitialDbState{}, err
	}

	static := staticRepoState{
		branch:          branch,
		RepoStateWriter: srcDb.DbData().Rsw,
		RepoStateReader: srcDb.DbData().Rsr,
	}

	remotes, err := static.GetRemotes()
	if err != nil {
		return dsess.InitialDbState{}, err
	}

	branches, err := static.GetBranches()
	if err != nil {
		return dsess.InitialDbState{}, err
	}

	backups, err := static.GetBackups()
	if err != nil {
		return dsess.InitialDbState{}, err
	}

	init := dsess.InitialDbState{
		Db:         srcDb,
		HeadCommit: cm,
		WorkingSet: ws,
		DbData: env.DbData{
			Ddb: srcDb.DbData().Ddb,
			Rsw: static,
			Rsr: static,
		},
		Remotes:  remotes,
		Branches: branches,
		Backups:  backups,
	}

	return init, nil
}

func revisionDbForTag(ctx context.Context, srcDb Database, revSpec string, requestedName string) (ReadOnlyDatabase, error) {
	baseName, _ := dsess.SplitRevisionDbName(srcDb.Name())
	return ReadOnlyDatabase{Database: Database{
		baseName:      baseName,
		requestedName: requestedName,
		ddb:           srcDb.DbData().Ddb,
		rsw:           srcDb.DbData().Rsw,
		rsr:           srcDb.DbData().Rsr,
		editOpts:      srcDb.editOpts,
		revision:      revSpec,
		revType:       dsess.RevisionTypeTag,
	}}, nil
}

func initialStateForTagDb(ctx context.Context, srcDb ReadOnlyDatabase) (dsess.InitialDbState, error) {
	revSpec := srcDb.Revision()
	tag := ref.NewTagRef(revSpec)

	cm, err := srcDb.DbData().Ddb.ResolveCommitRef(ctx, tag)
	if err != nil {
		return dsess.InitialDbState{}, err
	}

	init := dsess.InitialDbState{
		Db:         srcDb,
		HeadCommit: cm,
		ReadOnly:   true,
		DbData: env.DbData{
			Ddb: srcDb.DbData().Ddb,
			Rsw: srcDb.DbData().Rsw,
			Rsr: srcDb.DbData().Rsr,
		},
		// todo: should we initialize
		//  - Remotes
		//  - Branches
		//  - Backups
		//  - ReadReplicas
	}

	return init, nil
}

func revisionDbForCommit(ctx context.Context, srcDb Database, revSpec string, requestedName string) (ReadOnlyDatabase, error) {
	baseName, _ := dsess.SplitRevisionDbName(srcDb.Name())
	return ReadOnlyDatabase{Database: Database{
		baseName:      baseName,
		requestedName: requestedName,
		ddb:           srcDb.DbData().Ddb,
		rsw:           srcDb.DbData().Rsw,
		rsr:           srcDb.DbData().Rsr,
		editOpts:      srcDb.editOpts,
		revision:      revSpec,
		revType:       dsess.RevisionTypeCommit,
	}}, nil
}

func initialStateForCommit(ctx context.Context, srcDb ReadOnlyDatabase) (dsess.InitialDbState, error) {
	revSpec := srcDb.Revision()

	spec, err := doltdb.NewCommitSpec(revSpec)
	if err != nil {
		return dsess.InitialDbState{}, err
	}

	headRef, err := srcDb.DbData().Rsr.CWBHeadRef()
	if err != nil {
		return dsess.InitialDbState{}, err
	}
	cm, err := srcDb.DbData().Ddb.Resolve(ctx, spec, headRef)
	if err != nil {
		return dsess.InitialDbState{}, err
	}

	init := dsess.InitialDbState{
		Db:         srcDb,
		HeadCommit: cm,
		ReadOnly:   true,
		DbData: env.DbData{
			Ddb: srcDb.DbData().Ddb,
			Rsw: srcDb.DbData().Rsw,
			Rsr: srcDb.DbData().Rsr,
		},
		// todo: should we initialize
		//  - Remotes
		//  - Branches
		//  - Backups
		//  - ReadReplicas
	}

	return init, nil
}

type staticRepoState struct {
	branch ref.DoltRef
	env.RepoStateWriter
	env.RepoStateReader
}

func (s staticRepoState) CWBHeadRef() (ref.DoltRef, error) {
	return s.branch, nil
}

// formatDbMapKeyName returns formatted string of database name and/or branch name. Database name is case-insensitive,
// so it's stored in lower case name. Branch name is case-sensitive, so not changed.
// TODO: branch names should be case-insensitive too
func formatDbMapKeyName(name string) string {
	if !strings.Contains(name, dsess.DbRevisionDelimiter) {
		return strings.ToLower(name)
	}

	parts := strings.SplitN(name, dsess.DbRevisionDelimiter, 2)
	dbName, revSpec := parts[0], parts[1]

	return strings.ToLower(dbName) + dsess.DbRevisionDelimiter + revSpec
}
