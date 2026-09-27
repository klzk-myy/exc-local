// Package checks holds the checkpoint implementations bound to document
// checkpoint IDs (P<phase>-T<task>-C<n>). Golden-corpus cases live in
// package golden and are bound from here where they satisfy a checkpoint.
package checks

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	spec "exchange-testspec/spec"
)

// grepTree counts matches of pattern across *.go / *.hpp / *.cpp / *.txt /
// *.cmake files under root-relative dir (skip .git/build/vendor).
func grepTree(env *spec.Env, relDir, pattern string, exts ...string) (int, error) {
	if len(exts) == 0 {
		exts = []string{".go", ".hpp", ".cpp", ".h", ".txt", ".cmake"}
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return 0, err
	}
	root := env.Path(relDir)
	count := 0
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if name == ".git" || name == "build" || name == "build-debug" ||
				name == "vendor" || name == "third_party" || name == "node_modules" || name == "_deps" {
				return filepath.SkipDir
			}
			return nil
		}
		ok := false
		for _, e := range exts {
			if strings.HasSuffix(name, e) {
				ok = true
				break
			}
		}
		if !ok {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil // unreadable file is not evidence either way
		}
		count += len(re.FindAll(b, -1))
		return nil
	})
	return count, err
}

// seq runs results in order and returns the first non-pass; on all-pass it
// merges details so the report carries evidence from every sub-assertion.
func seq(parts ...spec.Result) spec.Result {
	var details []string
	for _, p := range parts {
		details = append(details, p.Detail)
		if p.Status != spec.StatusPass {
			return spec.Result{Status: p.Status, Detail: strings.Join(details, " | ")}
		}
	}
	return spec.Pass(strings.Join(details, " | "))
}

// seqf lazily evaluates steps, short-circuiting on the first non-pass.
type step func(context.Context, *spec.Env) spec.Result

func seqf(ctx context.Context, env *spec.Env, steps ...step) spec.Result {
	var details []string
	for _, s := range steps {
		p := s(ctx, env)
		details = append(details, p.Detail)
		if p.Status != spec.StatusPass {
			return spec.Result{Status: p.Status, Detail: strings.Join(details, " | ")}
		}
	}
	return spec.Pass(strings.Join(details, " | "))
}

// structural wraps a FileContains call for ctx-signature compatibility.
func structural(env *spec.Env, rel string, patterns ...string) step {
	return func(context.Context, *spec.Env) spec.Result {
		return spec.FileContains(env, rel, patterns...)
	}
}

func structuralLacks(env *spec.Env, rel string, patterns ...string) step {
	return func(context.Context, *spec.Env) spec.Result {
		return spec.FileLacks(env, rel, patterns...)
	}
}

func files(env *spec.Env, rels ...string) step {
	return func(context.Context, *spec.Env) spec.Result {
		return spec.RequireFiles(env, rels...)
	}
}

func ctest(regex string) step {
	return func(ctx context.Context, env *spec.Env) spec.Result {
		return spec.RunCTest(ctx, env, regex)
	}
}

// gtest runs one core gtest binary with a --gtest_filter suite pattern.
func gtest(binary, filter string) step {
	return func(ctx context.Context, env *spec.Env) spec.Result {
		return spec.RunGTest(ctx, env, binary, filter)
	}
}

func gotest(pkg, regex string, extra ...string) step {
	return func(ctx context.Context, env *spec.Env) spec.Result {
		return spec.RunGoTest(ctx, env, pkg, regex, extra...)
	}
}
