/usr/bin/docker run --rm -it -w /app -v /home/redacid/Projects/REDACID/kube-switch:/app:z -v /home/redacid/.cache/fyne-cross:/go:z --platform linux/amd64 --user 1000 -e HOME=/tmp -v /run/user/1000/keyring/ssh:/tmp/ssh-agent -e SSH_AUTH_SOCK=/tmp/ssh-agent -e CGO_ENABLED=1 -e GOCACHE=/go/go-build -e GOARCH=amd64 -e CC="zig cc -target x86_64-linux-gnu -isystem /usr/include -L/usr/lib/x86_64-linux-gnu" -e CXX="zig c++ -isystem /usr/include -target x86_64-linux-gnu -L/usr/lib/x86_64-linux-gnu" -e GOOS=linux docker.io/fyneio/fyne-cross-images:linux /bin/bash


fyne package -os linux -name kube-switch -icon /app/fyne-cross/tmp/linux-amd64/Icon.png -appBuild 79 -appVersion 0.0.2 -appID redacid.k8s.kube-switch -metadata Details.Version=0.0.2 -src ./cmd/kube-switch