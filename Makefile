SHELL=/bin/bash
# Go parameters
_GOCMD=go
_GOBUILD=$(_GOCMD) build
_GOCLEAN=$(_GOCMD) clean
_GOTEST=$(_GOCMD) test
_GOMOD=$(_GOCMD) mod

_REDHAT_REPO=scan.connect.redhat.com
_GITLAB_REPO=git.infinidat.com:4567
_BINARY_NAME=infinibox-csi-driver
_DOCKER_IMAGE=infinidat-csi-driver
_art_dir=artifact

# For Development Build #################################################################
# Docker.io username and tag
_DOCKER_USER=dohlemacher
#_DOCKER_IMAGE_TAG=test1
#_DOCKER_IMAGE_TAG=psdev-628-1
_DOCKER_IMAGE_TAG=redhat1

# redhat username and tag
_REDHAT_DOCKER_USER=user1
_REDHAT_DOCKER_IMAGE_TAG=rhtest1
# For Development Build #################################################################


# For Production Build ##################################################################
ifeq ($(env),prod)
	# For Production
	# Do not change following values unless change in production version or username
	#For docker.io  
	_DOCKER_USER=infinidat
	_DOCKER_IMAGE_TAG=1.1.0

	# For scan.connect.redhat.com
	_REDHAT_DOCKER_USER=ospid-956ccd64-1dcf-4d00-ba98-336497448906
	_REDHAT_DOCKER_IMAGE_TAG=1.1.0
endif
# For Production Build ##################################################################


clean:
	$(_GOCLEAN)
	rm -f $(_BINARY_NAME)

build:
	$(_GOBUILD) -o $(_BINARY_NAME) -v

test: 
	$(_GOTEST) -v ./...
  
run:
	$(_GOBUILD) -o $(_BINARY_NAME) -v ./...
	./$(_BINARY_NAME)

modverify:
	$(_GOMOD) verify

modtidy:
	$(_GOMOD) tidy

moddownload:
	$(_GOMOD) download

# Cross compilation
build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(_GOBUILD) -o $(_BINARY_NAME) -v

docker-build-docker: build
	docker build -t $(_DOCKER_USER)/$(_DOCKER_IMAGE):$(_DOCKER_IMAGE_TAG) -f Dockerfile .

docker-build-redhat: build
	docker build -t $(_REDHAT_REPO)/$(_REDHAT_DOCKER_USER)/$(_DOCKER_IMAGE):$(_REDHAT_DOCKER_IMAGE_TAG) -f Dockerfile-ubi .

docker-build-all: docker-build-docker docker-build-redhat

docker-push-docker:
	docker push $(_DOCKER_USER)/$(_DOCKER_IMAGE):$(_DOCKER_IMAGE_TAG)

docker-push-redhat:
	docker push $(_REDHAT_REPO)/$(_REDHAT_DOCKER_USER)/$(_DOCKER_IMAGE):$(_REDHAT_DOCKER_IMAGE_TAG)

docker-push-all: docker-push-docker docker-push-redhat

docker-push-gitlab-registry: docker-build-docker
	$(eval _TARGET_IMAGE=$(_GITLAB_REPO)/$(_DOCKER_USER)/$(_DOCKER_IMAGE):$(_DOCKER_IMAGE_TAG))
	docker login $(_GITLAB_REPO)
	docker tag $(_DOCKER_USER)/$(_DOCKER_IMAGE):$(_DOCKER_IMAGE_TAG) $(_TARGET_IMAGE) 
	docker push $(_TARGET_IMAGE)
	@#docker push $(_REDHAT_REPO)/$(_REDHAT_DOCKER_USER)/$(_DOCKER_IMAGE):$(_REDHAT_DOCKER_IMAGE_TAG)

buildlocal: build docker-build clean

all: build docker-build docker-push clean

docker-image-save:
	@# Save image to gzipped tar file to _art_dir.
	mkdir -p $(_art_dir) && \
	docker save $(_DOCKER_USER)/$(_DOCKER_IMAGE):$(_DOCKER_IMAGE_TAG) | gzip > ./$(_art_dir)/$(_DOCKER_IMAGE)_$(_DOCKER_IMAGE_TAG)_docker-image.tar.gz

docker-helm-chart-save:
	@# Save the helm chart to a tarball in _art_dir.
	mkdir -p $(_art_dir) && \
	tar cvfz ./$(_art_dir)/$(_DOCKER_IMAGE)_$(_DOCKER_IMAGE_TAG)_helm-chart.tar.gz deploy/helm
	@# --exclude='*.un~'

docker-save: docker-image-save docker-helm-chart-save
	@# Save the image and the helm chart to the _art_dir so that they may be provided to others.
