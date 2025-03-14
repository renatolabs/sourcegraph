# Install Sourcegraph with Docker Compose on Digital Ocean

This guide shows you how to deploy Sourcegraph via [Docker Compose](https://docs.docker.com/compose/) to a single Droplet running on DigitalOcean.

> NOTE: Trying to decide how to deploy Sourcegraph? See [our recommendations](../index.md) for how to choose a deployment type that suits your needs.

---
## Determine server and service requirements 

Use the [resource estimator](../resource_estimator.md) to determine the resource requirements for your environment. You will use this information to set up the instance and configure the docker-compose YAML file. 

## Prepare a fork 

We strongly recommend that you create and run Sourcegraph from your own fork of the reference repository. You will make changes to the default configuration, for example to the docker-compose YAML file, in your fork. The fork will also enable you to keep track of your customizations when upgrading your fork from the reference repo. Refer to the following steps for preparing a clone, which use GitHub as an example, then return to this page:

1. [Fork the reference repo](index.md#step-1-fork-the-sourcegraph-docker-compose-deployment-repository)
2. [Clone your fork](index.md#step-2-clone-the-forked-repository-locally)
3. [Configure the release branch](index.md#step-3-configure-the-release-branch)
4. [Configure the YAML file](index.md#step-4-configure-the-yaml-file)
5. [Publish changes to your branch](index.md#step-5-update-your-release-branch)

## Run Sourcegraph on a Digital Ocean Droplet

* [Create a new Digital Ocean Droplet](https://cloud.digitalocean.com/droplets/new).

  * Set the operating system to be **Ubuntu 18.04**.
  * For droplet size: use the [resource estimator](../resource_estimator.md) to find a good starting point for your deployment.
  * (**optional, recommended**) Set up SSH access (Authentication > SSH keys) for convenient access to the droplet.
  * (**optional, recommended**) Check the "Enable backups" checkbox to enable weekly backups of all your data.

* In the "Select additional options" section of the Droplet creation page, select the "User Data" and "Monitoring" boxes,
   and paste the following script in the "`Enter user data here...`" text box:

> NOTE: Replace the following variables in the script based on how you created your fork and release branch:
>
> * `DEPLOY_SOURCEGRAPH_DOCKER_FORK_CLONE_URL`: Your fork's git clone URL
> * `DEPLOY_SOURCEGRAPH_DOCKER_FORK_REVISION`: The git revision containing your fork's customizations to the base Sourcegraph Docker Compose YAML. In the [example](index.md#configure-a-release-branch) the revision is the `release` branch. 

```bash
#!/usr/bin/env bash

set -euxo pipefail

PERSISTENT_DISK_DEVICE_NAME='/dev/sda'
DOCKER_DATA_ROOT='/mnt/docker-data'

DOCKER_COMPOSE_VERSION='1.29.2'
DEPLOY_SOURCEGRAPH_DOCKER_CHECKOUT='/root/deploy-sourcegraph-docker'

# 🚨 Update these variables with the correct values from your fork!
DEPLOY_SOURCEGRAPH_DOCKER_FORK_CLONE_URL='https://github.com/sourcegraph/deploy-sourcegraph-docker.git'
DEPLOY_SOURCEGRAPH_DOCKER_FORK_REVISION='v3.43.2'

# Install git
sudo apt-get update -y
sudo apt-get install -y git

# Clone Docker Compose definition
git clone "${DEPLOY_SOURCEGRAPH_DOCKER_FORK_CLONE_URL}" "${DEPLOY_SOURCEGRAPH_DOCKER_CHECKOUT}"
cd "${DEPLOY_SOURCEGRAPH_DOCKER_CHECKOUT}"
git checkout "${DEPLOY_SOURCEGRAPH_DOCKER_FORK_REVISION}"

# Format (if necessary) and mount DO persistent disk
device_fs=$(sudo lsblk "${PERSISTENT_DISK_DEVICE_NAME}" --noheadings --output fsType)
if [ "${device_fs}" == "" ] ## only format the volume if it isn't already formatted
then
    sudo mkfs.ext4 -m 0 -E lazy_itable_init=0,lazy_journal_init=0,discard "${PERSISTENT_DISK_DEVICE_NAME}"
fi
sudo mkdir -p "${DOCKER_DATA_ROOT}"
sudo mount -o discard,defaults "${PERSISTENT_DISK_DEVICE_NAME}" "${DOCKER_DATA_ROOT}"

# Mount DO disk on reboots
DISK_UUID=$(sudo blkid -s UUID -o value "${PERSISTENT_DISK_DEVICE_NAME}")
sudo echo "UUID=${DISK_UUID}  ${DOCKER_DATA_ROOT}  ext4  discard,defaults,nofail  0  2" >> '/etc/fstab'
umount "${DOCKER_DATA_ROOT}"
mount -a

# Install Docker
curl -fsSL https://download.docker.com/linux/ubuntu/gpg | sudo apt-key add -
sudo add-apt-repository "deb [arch=amd64] https://download.docker.com/linux/ubuntu $(lsb_release -cs) stable"
sudo apt-get update -y
apt-cache policy docker-ce
apt-get install -y docker-ce docker-ce-cli containerd.io

# Install jq for scripting
sudo apt-get update -y
sudo apt-get install -y jq

# Edit Docker storage directory to mounted volume
DOCKER_DAEMON_CONFIG_FILE='/etc/docker/daemon.json'

## initialize the config file with empty json if it doesn't exist
if [ ! -f "${DOCKER_DAEMON_CONFIG_FILE}" ]
then
    mkdir -p $(dirname "${DOCKER_DAEMON_CONFIG_FILE}")
    echo '{}' > "${DOCKER_DAEMON_CONFIG_FILE}"
fi

## update Docker's 'data-root' to point to our mounted disk
tmp_config=$(mktemp)
trap "rm -f ${tmp_config}" EXIT
sudo cat "${DOCKER_DAEMON_CONFIG_FILE}" | sudo jq --arg DATA_ROOT "${DOCKER_DATA_ROOT}" '.["data-root"]=$DATA_ROOT' > "${tmp_config}"
sudo cat "${tmp_config}" > "${DOCKER_DAEMON_CONFIG_FILE}"

## finally, restart Docker daemon to pick up our changes
sudo systemctl restart --now docker

# Install Docker Compose
curl -L "https://github.com/docker/compose/releases/download/${DOCKER_COMPOSE_VERSION}/docker-compose-$(uname -s)-$(uname -m)" -o /usr/local/bin/docker-compose
chmod +x /usr/local/bin/docker-compose
curl -L "https://raw.githubusercontent.com/docker/compose/${DOCKER_COMPOSE_VERSION}/contrib/completion/bash/docker-compose" -o /etc/bash_completion.d/docker-compose

# Run Sourcegraph. Restart the containers upon reboot.
cd "${DEPLOY_SOURCEGRAPH_DOCKER_CHECKOUT}"/docker-compose
docker-compose up -d
```

* Click on "Add Volume" and add new block storage with the following settings:

  * **Size**: We recommend > 250 GB. *(As a rule of thumb, Sourcegraph needs at least as much space as all your repositories combined take up. Allocating as much disk space as you can upfront helps you avoid needing to select a droplet with a larger root disk later on.)*
  * Under **Chose configuration options**, select "Manually Format and Mount"

* You may have to wait a minute or two for the instance to finish initializing before Sourcegraph becomes accessible. You can monitor the status by SSHing into the Droplet and running the following diagnostic commands:

```bash
# Follow the status of the user data script you provided earlier
tail -f /var/log/cloud-init-output.log

# (Once the user data script completes) monitor the health of the "sourcegraph-frontend" container
docker ps --filter="name=sourcegraph-frontend-0"
```

* Navigate to the droplet's IP address to finish initializing Sourcegraph. If you have configured a DNS entry for the IP, configure `externalURL` to reflect that.

### After initialization

After initial setup, we recommend you do the following:

* Restrict the accessibility of ports other than `80` and `443` via [Cloud
  Firewalls](https://www.digitalocean.com/docs/networking/firewalls/quickstart/).
* Set up [TLS/SSL](../../../admin/http_https_configuration.md#sourcegraph-via-docker-compose-caddy-2)) in the Docker Compose deployment

---

## Update your Sourcegraph version

Refer to the [Docker Compose upgrade docs](upgrade.md).

## Storage and Backups

The [Sourcegraph Docker Compose definition](https://github.com/sourcegraph/deploy-sourcegraph-docker/blob/master/docker-compose/docker-compose.yaml) uses [Docker volumes](https://docs.docker.com/storage/volumes/) to store its data. The preceding script [configures Docker](https://docs.docker.com/engine/reference/commandline/dockerd/#daemon-configuration-file) to store all Docker data on the additional block storage volume that was attached to the droplet (mounted at `/mnt/docker-data` - the volumes themselves are stored under `/mnt/docker-data/volumes`) There are a few different ways to backup this data:

* (**recommended**) The most straightfoward method to backup this data is to [snapshot the entire `/mnt/docker-data` block storage volume on an automatic scheduled basis](https://www.digitalocean.com/docs/images/snapshots/).

* Using an external Postgres instance lets a service such as [Digital Ocean's Managed Database for Postgres](https://www.digitalocean.com/products/managed-databases-postgresql/) take care of backing up all of Sourcegraph's user data for you. If the droplet running Sourcegraph ever dies or is destroyed, creating a fresh droplet that's connected to that external Postgres will leave Sourcegraph in the same state that it was before.
