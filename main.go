package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/sirupsen/logrus"
	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
	"golang.org/x/sync/semaphore"
)

var (
	flagBuild string
	baseImage string
)

func init() {
	flag.StringVar(&flagBuild, "b", "", "Build all Dockerfiles under the specified directory")
	flag.StringVar(&baseImage, "image", "registry.redhat.io/rhel9/rhel-bootc:latest", "Base container image to analyze")
	flag.Parse()
}

func main() {
	if len(flagBuild) > 0 {
		if err := buildImages(flagBuild); err != nil {
			panic(err)
		}
	}

	packages, err := listAllPackages()
	if err != nil {
		panic(err)
	}

	if err := createDockerdockerfiles(packages); err != nil {
		panic(err)
	}
}

type rpmPackage struct {
	name       string
	arch       string
	version    string
	repository string
}

func listAllPackages() ([]rpmPackage, error) {
	p := mpb.New(mpb.WithWidth(64))
	bar := p.New(0,
		mpb.SpinnerStyle(".", "..", "...", "....", "").PositionLeft(),
		mpb.PrependDecorators(decor.Name("Listing available RPM packages")),
	)

	cmd := exec.Command("podman", "run", "--rm", baseImage,
		"dnf", "list", "--all", "--quiet", "--forcearch", "x86_64")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("listing packages in %s: %w\n%s", baseImage, err, output)
	}

	packages := make([]rpmPackage, 0, len(output))

	for _, out := range bytes.Split(output, []byte("\n")) {
		line := string(out)
		switch line { // filter known garbage logs
		case "Installed Packages", "Available Packages", "":
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("unexpected input with more than 3 fields: %q", line)
		}
		splt := strings.Split(fields[0], ".") // split off the architecture
		packages = append(packages, rpmPackage{
			name:       splt[0],
			arch:       splt[1],
			version:    fields[1],
			repository: strings.TrimLeft(fields[2], "@")}) // trim the @ off the repo
	}
	bar.Abort(true)
	bar.Wait()
	p.Wait()

	fmt.Printf("Found %d RPM packages\n", len(packages))
	return packages, nil
}

func createDockerdockerfiles(packages []rpmPackage) error {
	baseDir, err := os.MkdirTemp("", "RPM-Dockerfiles")
	if err != nil {
		return err
	}

	total := int64(len(packages))
	p := mpb.New(mpb.WithWidth(64))
	bar := p.AddBar(total,
		mpb.PrependDecorators(decor.Name("Writing Dockerfile for each package")),
	)

	written := 0
	for _, rpm := range packages {
		bar.Increment()
		baseName := fmt.Sprintf("%s.%s-%s-%s", rpm.name, rpm.arch, rpm.version, rpm.repository)

		dir := filepath.Join(baseDir, baseName)
		if err := os.Mkdir(dir, 0750); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return fmt.Errorf("creating directory for %q: %v", rpm.name, err)
		}

		dockerfile := fmt.Sprintf(`FROM %s
RUN dnf -y install %s-%s`, baseImage, rpm.name, rpm.version)

		if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0660); err != nil {
			return fmt.Errorf("writing Dockerfile for %q: %v", rpm.name, err)
		}
		written++
	}
	bar.Wait()
	p.Wait()

	fmt.Printf("Wrote %d Dockerfiles to %s\n", written, baseDir)
	return nil
}

func createDNFCache() (string, error) {
	p := mpb.New(mpb.WithWidth(64))
	bar := p.New(0,
		mpb.SpinnerStyle(".", "..", "...", "....", "").PositionLeft(),
		mpb.PrependDecorators(decor.Name("* Creating shared DNF cache")),
	)

	cacheDir, err := os.MkdirTemp("", "DNF-CACHE")
	if err != nil {
		return "", err
	}

	defer func() {
		bar.Abort(true)
		bar.Wait()
		p.Wait()
		fmt.Printf("* Created DNF cache directory: %s\n", cacheDir)
	}()

	cmd := exec.Command("podman", "run",
		"-v", fmt.Sprintf("%s:/var/cache/dnf", cacheDir),
		"--security-opt", "label=disable",
		baseImage, "dnf", "check-update")
	output, err := cmd.CombinedOutput()
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			if exitError.ExitCode() == 100 {
				return cacheDir, nil
			}
		}
		return "", fmt.Errorf("creating local DNF cache: %w (%s)", err, output)
	}

	return cacheDir, nil
}

func buildImages(dir string) error {
	cacheDir, err := createDNFCache()
	if err != nil {
		return err
	}

	dockerfiles := []string{}
	walkErr := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if !info.IsDir() {
			if info.Name() == "Dockerfile" {
				dockerfiles = append(dockerfiles, path)
			}
			return nil
		}
		return nil
	})

	sem := semaphore.NewWeighted(int64(4))
	ctx := context.Background()
	ctr := atomic.Uint64{}
	failedBuilds := []string{}
	for _, dockerfile := range dockerfiles {
		baseDir := filepath.Dir(dockerfile)
		logpath := filepath.Join(baseDir, "buildlog")

		if err := sem.Acquire(ctx, 1); err != nil {
			if err := os.WriteFile(logpath, []byte(fmt.Sprintf("%s", err)), 0660); err != nil {
				fmt.Printf(": failed writing build log")
			}
			continue
		}

		go func() {
			localCtr := ctr.Add(1)
			imageName := fmt.Sprintf("test-%d", localCtr)

			prefix := fmt.Sprintf("%d/%d Building %s", localCtr, len(dockerfiles), dockerfile)

			cmd := exec.Command("podman", "build", "-t", imageName,
				"-v", fmt.Sprintf("%s:/var/cache/dnf:O", cacheDir), baseDir)

			output, err := cmd.CombinedOutput()
			if err != nil {
				logpath += ".fail"
				fmt.Printf("%s: failed: see build log\n", prefix)
				failedBuilds = append(failedBuilds, dockerfile)
			} else {
				fmt.Printf("%s: success\n", prefix)
			}

			if err := os.WriteFile(logpath, []byte(fmt.Sprintf("%s: %s", err, output)), 0660); err != nil {
				logrus.Errorf("Writing buildlog for %s", dockerfile)
			}

			cmd = exec.Command("podman", "rmi", imageName)
			if _, err := cmd.CombinedOutput(); err != nil {
				logrus.Errorf("Removing image %s of %s: %s: %s", imageName, dockerfile, err, output)
			}
			sem.Release(1)
		}()
	}

	if len(failedBuilds) > 0 {
		fmt.Printf("The following builds failed:\n")
		for _, fail := range failedBuilds {
			fmt.Printf("* %s", fail)
		}
	}

	return walkErr
}
