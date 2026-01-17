.PHONY: build release test update-readme

build:
	go build -o ncp .

release:
	gox -osarch="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64" -output=release/ncp_{{.OS}}_{{.Arch}} 

test:
	go test ./...

update-readme:
	@echo "Building ncp..."
	@go build -o ncp . || (echo "Build failed!" && exit 1)
	@echo "Updating readme.md with latest help output..."
	@./ncp --help > /tmp/ncp-help.txt 2>&1 || (echo "Failed to get help output!" && exit 1)
	@awk 'BEGIN {in_details=0; help_printed=0} \
		/<details>/ {in_details=1; print; print ""; print "```"; print "$$ ./ncp --help"; while((getline line < "/tmp/ncp-help.txt") > 0) print line; close("/tmp/ncp-help.txt"); print "```"; print ""; help_printed=1; next} \
		in_details && /<\/details>/ {in_details=0; print; next} \
		in_details {next} \
		{print}' readme.md > readme.md.tmp && mv readme.md.tmp readme.md
	@rm -f /tmp/ncp-help.txt
	@echo "readme.md updated successfully!"
