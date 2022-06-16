// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/internal/event"
	"golang.org/x/tools/internal/gocommand"
	"golang.org/x/tools/internal/lsp/debug/tag"
	"golang.org/x/tools/internal/lsp/protocol"
	"golang.org/x/tools/internal/lsp/source"
	"golang.org/x/tools/internal/memoize"
	"golang.org/x/tools/internal/packagesinternal"
	"golang.org/x/tools/internal/span"
)

// load calls packages.Load for the given scopes, updating package metadata,
// import graph, and mapped files with the result.
//
// The resulting error may wrap the moduleErrorMap error type, representing
// errors associated with specific modules.
func (s *snapshot) load(ctx context.Context, allowNetwork bool, scopes ...interface{}) (err error) {
	var query []string
	var containsDir bool // for logging

	// Unless the context was canceled, set "shouldLoad" to false for all
	// of the metadata we attempted to load.
	defer func() {
		if errors.Is(err, context.Canceled) {
			return
		}
		s.clearShouldLoad(scopes...)
	}()

	// Keep track of module query -> module path so that we can later correlate query
	// errors with errors.
	moduleQueries := make(map[string]string)
	for _, scope := range scopes {
		if !s.shouldLoad(scope) {
			continue
		}
		switch scope := scope.(type) {
		case PackagePath:
			if source.IsCommandLineArguments(string(scope)) {
				panic("attempted to load command-line-arguments")
			}
			// The only time we pass package paths is when we're doing a
			// partial workspace load. In those cases, the paths came back from
			// go list and should already be GOPATH-vendorized when appropriate.
			query = append(query, string(scope))
		case fileURI:
			uri := span.URI(scope)
			// Don't try to load a file that doesn't exist.
			fh := s.FindFile(uri)
			if fh == nil || s.View().FileKind(fh) != source.Go {
				continue
			}
			query = append(query, fmt.Sprintf("file=%s", uri.Filename()))
		case moduleLoadScope:
			switch scope {
			case "std", "cmd":
				query = append(query, string(scope))
			default:
				modQuery := fmt.Sprintf("%s/...", scope)
				query = append(query, modQuery)
				moduleQueries[modQuery] = string(scope)
			}
		case viewLoadScope:
			// If we are outside of GOPATH, a module, or some other known
			// build system, don't load subdirectories.
			if !s.ValidBuildConfiguration() {
				query = append(query, "./")
			} else {
				query = append(query, "./...")
			}
		default:
			panic(fmt.Sprintf("unknown scope type %T", scope))
		}
		switch scope.(type) {
		case viewLoadScope, moduleLoadScope:
			containsDir = true
		}
	}
	if len(query) == 0 {
		return nil
	}
	sort.Strings(query) // for determinism

	if s.view.Options().VerboseWorkDoneProgress {
		work := s.view.session.progress.Start(ctx, "Load", fmt.Sprintf("Loading query=%s", query), nil, nil)
		defer func() {
			work.End(ctx, "Done.")
		}()
	}

	ctx, done := event.Start(ctx, "cache.view.load", tag.Query.Of(query))
	defer done()

	flags := source.LoadWorkspace
	if allowNetwork {
		flags |= source.AllowNetwork
	}
	_, inv, cleanup, err := s.goCommandInvocation(ctx, flags, &gocommand.Invocation{
		WorkingDir: s.view.rootURI.Filename(),
	})
	if err != nil {
		return err
	}

	// Set a last resort deadline on packages.Load since it calls the go
	// command, which may hang indefinitely if it has a bug. golang/go#42132
	// and golang/go#42255 have more context.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	cfg := s.config(ctx, inv)
	pkgs, err := packages.Load(cfg, query...)
	cleanup()

	// If the context was canceled, return early. Otherwise, we might be
	// type-checking an incomplete result. Check the context directly,
	// because go/packages adds extra information to the error.
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		event.Error(ctx, "go/packages.Load", err, tag.Snapshot.Of(s.ID()), tag.Directory.Of(cfg.Dir), tag.Query.Of(query), tag.PackageCount.Of(len(pkgs)))
	} else {
		event.Log(ctx, "go/packages.Load", tag.Snapshot.Of(s.ID()), tag.Directory.Of(cfg.Dir), tag.Query.Of(query), tag.PackageCount.Of(len(pkgs)))
	}
	if len(pkgs) == 0 {
		if err == nil {
			err = fmt.Errorf("no packages returned")
		}
		return fmt.Errorf("%v: %w", err, source.PackagesLoadError)
	}

	moduleErrs := make(map[string][]packages.Error) // module path -> errors
	updates := make(map[PackageID]*KnownMetadata)
	for _, pkg := range pkgs {
		// The Go command returns synthetic list results for module queries that
		// encountered module errors.
		//
		// For example, given a module path a.mod, we'll query for "a.mod/..." and
		// the go command will return a package named "a.mod/..." holding this
		// error. Save it for later interpretation.
		//
		// See golang/go#50862 for more details.
		if mod := moduleQueries[pkg.PkgPath]; mod != "" { // a synthetic result for the unloadable module
			if len(pkg.Errors) > 0 {
				moduleErrs[mod] = pkg.Errors
			}
			continue
		}

		if !containsDir || s.view.Options().VerboseOutput {
			event.Log(ctx, "go/packages.Load",
				tag.Snapshot.Of(s.ID()),
				tag.Package.Of(pkg.ID),
				tag.Files.Of(pkg.CompiledGoFiles))
		}
		// Ignore packages with no sources, since we will never be able to
		// correctly invalidate that metadata.
		if len(pkg.GoFiles) == 0 && len(pkg.CompiledGoFiles) == 0 {
			continue
		}
		// Special case for the builtin package, as it has no dependencies.
		if pkg.PkgPath == "builtin" {
			if len(pkg.GoFiles) != 1 {
				return fmt.Errorf("only expected 1 file for builtin, got %v", len(pkg.GoFiles))
			}
			s.setBuiltin(pkg.GoFiles[0])
			continue
		}
		// Skip test main packages.
		if isTestMain(pkg, s.view.gocache) {
			continue
		}
		// Skip filtered packages. They may be added anyway if they're
		// dependencies of non-filtered packages.
		if s.view.allFilesExcluded(pkg) {
			continue
		}
		// TODO: once metadata is immutable, we shouldn't have to lock here.
		s.mu.Lock()
		err := s.computeMetadataUpdates(ctx, PackagePath(pkg.PkgPath), pkg, cfg, query, updates, nil)
		s.mu.Unlock()
		if err != nil {
			return err
		}
	}

	s.mu.Lock()
	s.meta = s.meta.Clone(updates)
	// Invalidate any packages we may have associated with this metadata.
	//
	// TODO(rfindley): if we didn't already invalidate these in snapshot.clone,
	// shouldn't we invalidate the reverse transitive closure?
	for _, m := range updates {
		for _, mode := range []source.ParseMode{source.ParseHeader, source.ParseExported, source.ParseFull} {
			key := packageKey{mode, m.ID}
			delete(s.packages, key)
		}
	}
	s.workspacePackages = computeWorkspacePackages(s.meta)
	s.dumpWorkspace("load")
	s.mu.Unlock()

	// Rebuild the workspace package handle for any packages we invalidated.
	//
	// TODO(rfindley): what's the point of returning an error here? Probably we
	// can simply remove this step: The package handle will be rebuilt as needed.
	for _, m := range updates {
		if _, err := s.buildPackageHandle(ctx, m.ID, s.workspaceParseMode(m.ID)); err != nil {
			return err
		}
	}

	if len(moduleErrs) > 0 {
		return &moduleErrorMap{moduleErrs}
	}

	return nil
}

type moduleErrorMap struct {
	errs map[string][]packages.Error // module path -> errors
}

func (m *moduleErrorMap) Error() string {
	var paths []string // sort for stability
	for path, errs := range m.errs {
		if len(errs) > 0 { // should always be true, but be cautious
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%d modules have errors:\n", len(paths))
	for _, path := range paths {
		fmt.Fprintf(&buf, "\t%s", m.errs[path][0].Msg)
	}

	return buf.String()
}

// workspaceLayoutErrors returns a diagnostic for every open file, as well as
// an error message if there are no open files.
func (s *snapshot) workspaceLayoutError(ctx context.Context) *source.CriticalError {
	if len(s.workspace.getKnownModFiles()) == 0 {
		return nil
	}
	if s.view.userGo111Module == off {
		return nil
	}
	if s.workspace.moduleSource != legacyWorkspace {
		return nil
	}
	// If the user has one module per view, there is nothing to warn about.
	if s.ValidBuildConfiguration() && len(s.workspace.getKnownModFiles()) == 1 {
		return nil
	}

	// Apply diagnostics about the workspace configuration to relevant open
	// files.
	openFiles := s.openFiles()

	// If the snapshot does not have a valid build configuration, it may be
	// that the user has opened a directory that contains multiple modules.
	// Check for that an warn about it.
	if !s.ValidBuildConfiguration() {
		msg := `gopls requires a module at the root of your workspace.
You can work with multiple modules by opening each one as a workspace folder.
Improvements to this workflow will be coming soon, and you can learn more here:
https://github.com/golang/tools/blob/master/gopls/doc/workspace.md.`
		return &source.CriticalError{
			MainError: fmt.Errorf(msg),
			DiagList:  s.applyCriticalErrorToFiles(ctx, msg, openFiles),
		}
	}

	// If the user has one active go.mod file, they may still be editing files
	// in nested modules. Check the module of each open file and add warnings
	// that the nested module must be opened as a workspace folder.
	if len(s.workspace.getActiveModFiles()) == 1 {
		// Get the active root go.mod file to compare against.
		var rootModURI span.URI
		for uri := range s.workspace.getActiveModFiles() {
			rootModURI = uri
		}
		nestedModules := map[string][]source.VersionedFileHandle{}
		for _, fh := range openFiles {
			modURI := moduleForURI(s.workspace.knownModFiles, fh.URI())
			if modURI != rootModURI {
				modDir := filepath.Dir(modURI.Filename())
				nestedModules[modDir] = append(nestedModules[modDir], fh)
			}
		}
		// Add a diagnostic to each file in a nested module to mark it as
		// "orphaned". Don't show a general diagnostic in the progress bar,
		// because the user may still want to edit a file in a nested module.
		var srcDiags []*source.Diagnostic
		for modDir, uris := range nestedModules {
			msg := fmt.Sprintf(`This file is in %s, which is a nested module in the %s module.
gopls currently requires one module per workspace folder.
Please open %s as a separate workspace folder.
You can learn more here: https://github.com/golang/tools/blob/master/gopls/doc/workspace.md.
`, modDir, filepath.Dir(rootModURI.Filename()), modDir)
			srcDiags = append(srcDiags, s.applyCriticalErrorToFiles(ctx, msg, uris)...)
		}
		if len(srcDiags) != 0 {
			return &source.CriticalError{
				MainError: fmt.Errorf(`You are working in a nested module.
Please open it as a separate workspace folder. Learn more:
https://github.com/golang/tools/blob/master/gopls/doc/workspace.md.`),
				DiagList: srcDiags,
			}
		}
	}
	return nil
}

func (s *snapshot) applyCriticalErrorToFiles(ctx context.Context, msg string, files []source.VersionedFileHandle) []*source.Diagnostic {
	var srcDiags []*source.Diagnostic
	for _, fh := range files {
		// Place the diagnostics on the package or module declarations.
		var rng protocol.Range
		switch s.view.FileKind(fh) {
		case source.Go:
			if pgf, err := s.ParseGo(ctx, fh, source.ParseHeader); err == nil {
				pkgDecl := span.NewRange(s.FileSet(), pgf.File.Package, pgf.File.Name.End())
				if spn, err := pkgDecl.Span(); err == nil {
					rng, _ = pgf.Mapper.Range(spn)
				}
			}
		case source.Mod:
			if pmf, err := s.ParseMod(ctx, fh); err == nil {
				if pmf.File.Module != nil && pmf.File.Module.Syntax != nil {
					rng, _ = rangeFromPositions(pmf.Mapper, pmf.File.Module.Syntax.Start, pmf.File.Module.Syntax.End)
				}
			}
		}
		srcDiags = append(srcDiags, &source.Diagnostic{
			URI:      fh.URI(),
			Range:    rng,
			Severity: protocol.SeverityError,
			Source:   source.ListError,
			Message:  msg,
		})
	}
	return srcDiags
}

type workspaceDirKey string

type workspaceDirData struct {
	dir string
	err error
}

// getWorkspaceDir gets the URI for the workspace directory associated with
// this snapshot. The workspace directory is a temp directory containing the
// go.mod file computed from all active modules.
func (s *snapshot) getWorkspaceDir(ctx context.Context) (span.URI, error) {
	s.mu.Lock()
	h := s.workspaceDirHandle
	s.mu.Unlock()
	if h != nil {
		return getWorkspaceDir(ctx, h, s.generation)
	}
	file, err := s.workspace.modFile(ctx, s)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	modContent, err := file.Format()
	if err != nil {
		return "", err
	}
	sumContent, err := s.workspace.sumFile(ctx, s)
	if err != nil {
		return "", err
	}
	hash.Write(modContent)
	hash.Write(sumContent)
	key := workspaceDirKey(hash.Sum(nil))
	s.mu.Lock()
	h = s.generation.Bind(key, func(context.Context, memoize.Arg) interface{} {
		tmpdir, err := ioutil.TempDir("", "gopls-workspace-mod")
		if err != nil {
			return &workspaceDirData{err: err}
		}

		for name, content := range map[string][]byte{
			"go.mod": modContent,
			"go.sum": sumContent,
		} {
			filename := filepath.Join(tmpdir, name)
			if err := ioutil.WriteFile(filename, content, 0644); err != nil {
				os.RemoveAll(tmpdir)
				return &workspaceDirData{err: err}
			}
		}

		return &workspaceDirData{dir: tmpdir}
	}, func(v interface{}) {
		d := v.(*workspaceDirData)
		if d.dir != "" {
			if err := os.RemoveAll(d.dir); err != nil {
				event.Error(context.Background(), "cleaning workspace dir", err)
			}
		}
	})
	s.workspaceDirHandle = h
	s.mu.Unlock()
	return getWorkspaceDir(ctx, h, s.generation)
}

func getWorkspaceDir(ctx context.Context, h *memoize.Handle, g *memoize.Generation) (span.URI, error) {
	v, err := h.Get(ctx, g, nil)
	if err != nil {
		return "", err
	}
	return span.URIFromPath(v.(*workspaceDirData).dir), nil
}

// computeMetadataUpdates populates the updates map with metadata updates to
// apply, based on the given pkg. It recurs through pkg.Imports to ensure that
// metadata exists for all dependencies.
func (s *snapshot) computeMetadataUpdates(ctx context.Context, pkgPath PackagePath, pkg *packages.Package, cfg *packages.Config, query []string, updates map[PackageID]*KnownMetadata, path []PackageID) error {
	id := PackageID(pkg.ID)
	if new := updates[id]; new != nil {
		return nil
	}
	if source.IsCommandLineArguments(pkg.ID) {
		suffix := ":" + strings.Join(query, ",")
		id = PackageID(string(id) + suffix)
		pkgPath = PackagePath(string(pkgPath) + suffix)
	}
	if _, ok := updates[id]; ok {
		// If we've already seen this dependency, there may be an import cycle, or
		// we may have reached the same package transitively via distinct paths.
		// Check the path to confirm.
		for _, prev := range path {
			if prev == id {
				return fmt.Errorf("import cycle detected: %q", id)
			}
		}
	}
	// Recreate the metadata rather than reusing it to avoid locking.
	m := &KnownMetadata{
		Metadata: &Metadata{
			ID:         id,
			PkgPath:    pkgPath,
			Name:       PackageName(pkg.Name),
			ForTest:    PackagePath(packagesinternal.GetForTest(pkg)),
			TypesSizes: pkg.TypesSizes,
			Config:     cfg,
			Module:     pkg.Module,
			depsErrors: packagesinternal.GetDepsErrors(pkg),
		},
		Valid: true,
	}
	updates[id] = m

	// Identify intermediate test variants for later filtering. See the
	// documentation of IsIntermediateTestVariant for more information.
	if m.ForTest != "" && m.ForTest != m.PkgPath && m.ForTest+"_test" != m.PkgPath {
		m.IsIntermediateTestVariant = true
	}

	for _, err := range pkg.Errors {
		// Filter out parse errors from go list. We'll get them when we
		// actually parse, and buggy overlay support may generate spurious
		// errors. (See TestNewModule_Issue38207.)
		if strings.Contains(err.Msg, "expected '") {
			continue
		}
		m.Errors = append(m.Errors, err)
	}

	uris := map[span.URI]struct{}{}
	for _, filename := range pkg.CompiledGoFiles {
		uri := span.URIFromPath(filename)
		m.CompiledGoFiles = append(m.CompiledGoFiles, uri)
		uris[uri] = struct{}{}
	}
	for _, filename := range pkg.GoFiles {
		uri := span.URIFromPath(filename)
		m.GoFiles = append(m.GoFiles, uri)
		uris[uri] = struct{}{}
	}

	for uri := range uris {
		// In order for a package to be considered for the workspace, at least one
		// file must be contained in the workspace and not vendored.

		// The package's files are in this view. It may be a workspace package.
		// Vendored packages are not likely to be interesting to the user.
		if !strings.Contains(string(uri), "/vendor/") && s.view.contains(uri) {
			m.HasWorkspaceFiles = true
			break
		}
	}

	for importPath, importPkg := range pkg.Imports {
		importPkgPath := PackagePath(importPath)
		importID := PackageID(importPkg.ID)

		m.Deps = append(m.Deps, importID)

		// Don't remember any imports with significant errors.
		if importPkgPath != "unsafe" && len(importPkg.CompiledGoFiles) == 0 {
			if m.MissingDeps == nil {
				m.MissingDeps = make(map[PackagePath]struct{})
			}
			m.MissingDeps[importPkgPath] = struct{}{}
			continue
		}
		if s.noValidMetadataForIDLocked(importID) {
			if err := s.computeMetadataUpdates(ctx, importPkgPath, importPkg, cfg, query, updates, append(path, id)); err != nil {
				event.Error(ctx, "error in dependency", err)
			}
		}
	}

	return nil
}

// computeWorkspacePackages computes workspace packages for the given metadata
// graph.
func computeWorkspacePackages(meta *metadataGraph) map[PackageID]PackagePath {
	workspacePackages := make(map[PackageID]PackagePath)
	for _, m := range meta.metadata {
		if !m.HasWorkspaceFiles {
			continue
		}
		if m.PkgFilesChanged {
			// If a package name has changed, it's possible that the package no
			// longer exists. Leaving it as a workspace package can result in
			// persistent stale diagnostics.
			//
			// If there are still valid files in the package, it will be reloaded.
			//
			// There may be more precise heuristics.
			continue
		}

		if source.IsCommandLineArguments(string(m.ID)) {
			// If all the files contained in m have a real package, we don't need to
			// keep m as a workspace package.
			if allFilesHaveRealPackages(meta, m) {
				continue
			}
		}

		switch {
		case m.ForTest == "":
			// A normal package.
			workspacePackages[m.ID] = m.PkgPath
		case m.ForTest == m.PkgPath, m.ForTest+"_test" == m.PkgPath:
			// The test variant of some workspace package or its x_test.
			// To load it, we need to load the non-test variant with -test.
			workspacePackages[m.ID] = m.ForTest
		}
	}
	return workspacePackages
}

// allFilesHaveRealPackages reports whether all files referenced by m are
// contained in a "real" package (not command-line-arguments).
//
// If m is valid but all "real" packages containing any file are invalid, this
// function returns false.
//
// If m is not a command-line-arguments package, this is trivially true.
func allFilesHaveRealPackages(g *metadataGraph, m *KnownMetadata) bool {
	n := len(m.CompiledGoFiles)
checkURIs:
	for _, uri := range append(m.CompiledGoFiles[0:n:n], m.GoFiles...) {
		for _, id := range g.ids[uri] {
			if !source.IsCommandLineArguments(string(id)) && (g.metadata[id].Valid || !m.Valid) {
				continue checkURIs
			}
		}
		return false
	}
	return true
}

func isTestMain(pkg *packages.Package, gocache string) bool {
	// Test mains must have an import path that ends with ".test".
	if !strings.HasSuffix(pkg.PkgPath, ".test") {
		return false
	}
	// Test main packages are always named "main".
	if pkg.Name != "main" {
		return false
	}
	// Test mains always have exactly one GoFile that is in the build cache.
	if len(pkg.GoFiles) > 1 {
		return false
	}
	if !source.InDir(gocache, pkg.GoFiles[0]) {
		return false
	}
	return true
}
