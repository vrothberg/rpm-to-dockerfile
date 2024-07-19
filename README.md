# rpm-to-dockerfile

A tool to analyze all available RPM packages inside a given container image.
For each package, it will create a Dockerfile to `dnf -y install --allowerasing` the specific package.

To create the Dockerfiles run:
```
go run main.go
Found 6842 RPM packages                                                                             
Writing Dockerfile for each package [==============================================================]
Wrote 6832 Dockerfiles to /tmp/RPM-Dockerfiles3159832664                                            
```
Some of the intial 6842 packages are duplicates which are filtered before writing Dockerfiles.

Each Dockerfile is writtin in a specific directory indicating the package name, version and repository, for instance `/tmp/RPM-Dockerfiles3159832664/zstd.x86_64-1.5.1-2.el9-System`.

To build the Dockerfiles, run `go main.go -dir /tmp/RPM-Dockerfiles3159832664`.
It will first create a shared DNF cache to speed up builds and run 4 builds in parallel.
You can use the `-j N` flag to change the number of parallel builds.

At the moment, the default image to analyze is `registry.redhat.io/rhel9/rhel-bootc:latest`.
You can use the `-image` flag to use another one.
The build logs will be placed in the specific directory next to the Dockerfile.
