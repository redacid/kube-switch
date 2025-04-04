GOTOOLCHAIN=local
GOPATH=/go
MINISIGN_VERSION=0.12
FIXUID_VERSION=0.5.1
FYNE_VERSION=v2.5.3
ZIG_VERSION=0.13.0
GO_VERSION=1.23.5

apt-get update;
apt-get install -y -q --no-install-recommends ca-certificates curl git pkg-config unzip xz-utils zip;
		 apt-get -qy autoremove;
		 apt-get clean;
		 rm -r /var/lib/apt/lists/*;

arch="$(dpkg --print-architecture)";
url=;
sha256=;
case "$arch" in
     'amd64')
		    url="https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz";
		    sha256=${GO_AMD64_CHECKSUM};
		 ;;
     'arm64')
		    url="https://go.dev/dl/go${GO_VERSION}.linux-arm64.tar.gz";
		    sha256=${GO_ARM64_CHECKSUM};
		 ;;
     *) echo >&2 "error: unsupported architecture '$arch'";
        exit 1 ;;
esac;
curl -sSL ${url} -o go.tgz;
echo ${sha256} go.tgz | sha256sum -c -;
tar -C /usr/local -zxf go.tgz;
   rm go.tgz;
   go version;

mkdir -p "$GOPATH/src" "$GOPATH/bin" && chmod -R 777 "$GOPATH"

curl -sSL https://github.com/jedisct1/minisign/releases/download/${MINISIGN_VERSION}/minisign-${MINISIGN_VERSION}-linux.tar.gz -o minisign.tgz;
mkdir -p /usr/local/minisign;
ls -l;
tar -C /usr/local/minisign --strip-components=1 -zxvf minisign.tgz;
rm minisign.tgz

arch="$(dpkg --print-architecture)";
url=;
public_key=${ZIG_MINISIGN_KEY};
case "$arch" in         'amd64')             arch="x86_64";
url="https://ziglang.org/download/${ZIG_VERSION}/zig-linux-${arch}-${ZIG_VERSION}.tar.xz";
            ;;
			'arm64')
			arch="aarch64";
            url="https://ziglang.org/download/${ZIG_VERSION}/zig-linux-${arch}-${ZIG_VERSION}.tar.xz";
            ;;
			*) echo >&2 "error: unsupported architecture '$arch'";
			exit 1 ;;
			esac;
			curl -sSL ${url} -o zig.tar.xz;
			curl -sSL ${url}.minisig -o zig.tar.xz.minisig;
			/usr/local/minisign/${arch}/minisign -Vm zig.tar.xz -P ${public_key};
			tar -C  /usr/local -Jxvf zig.tar.xz;
			mv /usr/local/zig-* /usr/local/zig;
			rm zig.tar*;
			zig version;


go install -ldflags="-w -s" -v "github.com/fyne-io/fyne-cross/internal/cmd/fyne-cross-s3@develop";
mv /go/bin/fyne-cross-s3 /usr/local/bin/fyne-cross-s3; # buildkit

go install -ldflags="-w -s" -v "fyne.io/fyne/v2/cmd/fyne@${FYNE_VERSION}";
mv /go/bin/fyne /usr/local/bin/fyne;
fyne version;
go clean -cache -modcache;
mkdir -p "$GOPATH/pkg/mod" && chmod -R 777 "$GOPATH" # buildkit

git config --global --add safe.directory /app && mv ~/.gitconfig /etc/gitconfig && chmod a+r /etc/gitconfig

arch="$(dpkg --print-architecture)";
addgroup --gid 1000 docker;
adduser --uid 1000 --ingroup docker --home /home/docker --shell /bin/sh --disabled-password --gecos "" docker;
curl -SsL https://github.com/boxboat/fixuid/releases/download/v${FIXUID_VERSION}/fixuid-${FIXUID_VERSION}-linux-${arch}.tar.gz | tar -C /usr/local/bin-xzf -;
chown root:root /usr/local/bin/fixuid;
chmod 4755 /usr/local/bin/fixuid;
mkdir -p /etc/fixuid;
printf "user: docker\ngroup: docker\n" > /etc/fixuid/config.yml

dpkg --add-architecture arm64;
dpkg --add-architecture amd64;
dpkg --add-architecture armhf;
dpkg --add-architecture i386;
apt-get update;
apt-get install -y -q --no-install-recommends libgl-dev:amd64 libx11-dev:amd64 libxrandr-dev:amd64 libxxf86vm-dev:amd64 libxi-dev:amd64 libxcursor-dev:amd64 libxinerama-dev:amd64 libxkbcommon-dev:amd64 libdecor-0-dev:amd64;
apt-get install -y -q --no-install-recommends libgl-dev:arm64 libx11-dev:arm64 libxrandr-dev:arm64 libxxf86vm-dev:arm64 libxi-dev:arm64 libxcursor-dev:arm64 libxinerama-dev:arm64 libxkbcommon-dev:arm64 libdecor-0-dev:arm64;
apt-get install -y -q --no-install-recommends libgl-dev:armhf libx11-dev:armhf libxrandr-dev:armhf libxxf86vm-dev:armhf libxi-dev:armhf libxcursor-dev:armhf libxinerama-dev:armhf libxkbcommon-dev:armhf libdecor-0-dev:armhf;
apt-get install -y -q --no-install-recommends libgl-dev:i386