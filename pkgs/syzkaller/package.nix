# syzkaller, pinned to an upstream commit rather than taken from nixpkgs.
#
# nixpkgs' copy is stuck on a January 2024 revision. Syscall descriptions move
# quickly — they are the whole of what a fuzzer can reach — and that snapshot
# misses a great deal of kernel surface. To bump this, edit `rev`, `hash` and
# `vendorHash` together.
#
# Every comment below is a failure that was debugged once and should not be
# debugged again. None of them produce a build error: they all produce a
# fuzzer that starts, runs, and quietly does less than it should.
{
  lib,
  buildGoModule,
  fetchFromGitHub,
  ncurses,
  glibc,
}:

buildGoModule (finalAttrs: {
  pname = "syzkaller";
  version = "0-unstable-2026-08-04";

  src = fetchFromGitHub {
    owner = "google";
    repo = "syzkaller";
    rev = "f775a85c97a8e4121f8160648358bdd772180846";
    hash = "sha256-2jS/b8zsD9xg6VnW4WEwynWiTo54QIWT4ttDeHi3CxU=";
  };

  vendorHash = "sha256-FOA28TsLgk1jB1UJnn5TNd8CQ1gkY0MdDe4V76Z7bBM=";

  # `tput` is called unconditionally at Makefile parse time for colour codes.
  nativeBuildInputs = [ ncurses ];

  # NB: glibc.static is deliberately NOT a buildInput. Putting it there adds it
  # to the global link flags, which makes the Go toolchain attempt a static cgo
  # link and fail with a wall of "relocation target ... not defined". It is
  # referenced only in the executor's own link line, in buildPhase.

  # buildGoModule normally passes -trimpath via GOFLAGS, but we have to unset
  # that (see buildPhase), and without -trimpath the binaries embed build paths
  # and keep a reference to the Go toolchain -- which disallowedReferences then
  # rejects, and which would drag ~250 MiB into the closure. Inject it into the
  # Makefile's own flag set instead. Deliberately --replace-fail: if upstream
  # reshuffles this line the build stops rather than silently regressing.
  postPatch = ''
    substituteInPlace Makefile \
      --replace-fail 'GOFLAGS := -ldflags=' 'GOFLAGS := -trimpath -ldflags='
  '';

  # Silences the "run command via tools/syz-env" warning banner, which the
  # Makefile emits whenever SYZ_ENV is unset.
  SYZ_ENV = "1";

  makeFlags = [
    "TARGETOS=linux"
    "TARGETARCH=amd64"
    "TARGETVMARCH=amd64"
    "HOSTOS=linux"
    "HOSTARCH=amd64"
    # tools/version.mk guards both of these behind `ifeq ($(VAR),)`, so setting
    # them here avoids the git shell-outs entirely -- no Makefile patching, and
    # nothing to re-fix when upstream reshuffles the version plumbing.
    "REV=${finalAttrs.src.rev}"
    "GITREVDATE=20260804-133700"
    # The Makefile says `CGO_ENABLED ?= 0` to get static Go binaries, but
    # buildGoModule exports CGO_ENABLED=1, and ?= defers to the environment. The
    # result is a dynamically linked syz-execprog -- which, like syz-executor,
    # is copied into the guest and cannot resolve a nix-store interpreter there.
    # A command-line variable overrides both the environment and ?=.
    "CGO_ENABLED=0"
  ];

  # `make all` additionally runs check_syzos, an upstream self-test that is not
  # needed to fuzz. These targets cover host tooling + the in-VM binaries.
  buildPhase = ''
    runHook preBuild

    # buildGoModule exports GOFLAGS=-mod=vendor, but syzkaller's Makefile also
    # assigns GOFLAGS (holding -ldflags="-X ..."). Because the name arrived from
    # the environment, make re-exports the *Makefile's* value, and the go tool
    # then fails parsing it: "unknown flag -X". Dropping it restores upstream's
    # own behaviour -- the Makefile passes its flags on the command line, and Go
    # selects vendor mode on its own because vendor/ exists and go.mod is >=1.14.
    unset GOFLAGS

    # syz-executor is copied into the guest and run there. Linked normally it
    # points at the nix-store glibc, whose ELF interpreter does not exist inside
    # the guest; the guest then fails with the rather opaque
    # "/syz-executor: cannot execute: required file not found" and syz-manager
    # loops on "lost connection to test machine". Linking it statically makes it
    # independent of whatever userspace the guest image happens to be.
    # -static-pie rather than -static because nixpkgs' cc-wrapper adds -pie via
    # its default hardening flags, which plain -static conflicts with. The Go
    # binaries are already static -- the Makefile forces CGO_ENABLED=0.
    #
    # Passed via LDFLAGS, not ADDCXXFLAGS: the executor recipe is
    # `$(ADDCXXFLAGS) $(CXXFLAGS) $(LDFLAGS)`, and ADDCXXFLAGS already carries
    # the real compiler flags (-std=c++17, -I., -O2 ...). Overriding it on the
    # command line drops those and the build dies on a missing flatrpc.h.
    # LDFLAGS is empty here, so it is the safe place to add to.
    make $makeFlags manager repro mutate prog2c db upgrade symbolize crush \
      execprog executor \
      LDFLAGS="-static-pie -B${glibc.static}/lib -L${glibc.static}/lib"

    runHook postBuild
  '';

  # Layout matters: syz-manager resolves the executor relative to the directory
  # named by `syzkaller` in the manager config, expecting
  # <dir>/bin/linux_amd64/syz-executor. Preserving ./bin verbatim under $out
  # means that field can just point at this store path.
  installPhase = ''
    runHook preInstall

    mkdir -p $out
    cp -r bin $out/

    runHook postInstall
  '';

  # The Go binaries are static (the Makefile forces CGO_ENABLED=0) and the
  # executor is a single C++ translation unit; nothing here needs a Go test run
  # during the build.
  doCheck = false;

  meta = {
    description = "Unsupervised, coverage-guided kernel fuzzer";
    homepage = "https://github.com/google/syzkaller";
    license = lib.licenses.asl20;
    platforms = lib.platforms.linux;
    mainProgram = "syz-manager";
  };
})
