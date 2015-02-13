package integration

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	boshsettings "github.com/cloudfoundry/bosh-agent/settings"
	boshsys "github.com/cloudfoundry/bosh-agent/system"
)

type TestEnvironment struct {
	cmdRunner        boshsys.CmdRunner
	currentDeviceNum int
}

func NewTestEnvironment(
	cmdRunner boshsys.CmdRunner,
) *TestEnvironment {
	return &TestEnvironment{
		cmdRunner:        cmdRunner,
		currentDeviceNum: 2,
	}
}

func (t *TestEnvironment) SetupConfigDrive() error {
	loopDeviceNum, err := t.AttachLoopDevice(10)
	if err != nil {
		return err
	}

	setupConfigDriveTemplate := `
sudo mkfs -t ext3 -m 1 -v /dev/loop%d
sudo e2label /dev/loop%d config-2
sudo rm -rf /tmp/config-drive
sudo mkdir /tmp/config-drive
sudo mount /dev/disk/by-label/config-2 /tmp/config-drive
sudo chown vagrant:vagrant /tmp/config-drive
sudo mkdir -p /tmp/config-drive/ec2/latest
sudo cp %s/meta-data.json /tmp/config-drive/ec2/latest/meta-data.json
sudo cp %s/user-data.json /tmp/config-drive/ec2/latest/user-data.json
sudo umount /tmp/config-drive
`
	setupConfigDriveScript := fmt.Sprintf(setupConfigDriveTemplate, loopDeviceNum, loopDeviceNum, t.assetsDir(), t.assetsDir())

	_, err = t.RunCommand(setupConfigDriveScript)
	return err
}

func (t *TestEnvironment) CleanupDataDir() error {
	t.RunCommand(`sudo /var/vcap/bosh/bin/monit stop all`)

	mountPoints, err := t.RunCommand(`sudo mount | grep "on /var/vcap/data" | cut -d ' ' -f 3`)
	if err != nil {
		return err
	}

	for _, mountPoint := range strings.Split(mountPoints, "\n") {
		if mountPoint != "" {
			t.RunCommand(fmt.Sprintf("sudo umount -l %s", mountPoint))
		}
	}

	_, err = t.RunCommand("sudo rm -rf /var/vcap/data")
	return err
}

// Until bosh-lite is released with new agent
func (t *TestEnvironment) ConfigureAgentForGenericInfrastructure() error {
	_, err := t.RunCommand(
		fmt.Sprintf(
			"sudo cp %s/agent_runit.sh /etc/service/agent/run",
			t.assetsDir(),
		),
	)
	return err
}

func (t *TestEnvironment) CleanupLogFile() error {
	_, err := t.RunCommand("sudo truncate -s 0 /var/vcap/bosh/log/current")
	return err
}

func (t *TestEnvironment) LogFileContains(content string) bool {
	_, err := t.RunCommand(fmt.Sprintf(`sudo grep "%s" /var/vcap/bosh/log/current`, content))
	return err == nil
}

func (t *TestEnvironment) AttachDevice(devicePath string, partitionSize, numPartitions int) error {
	partitionPath := devicePath
	for i := 0; i <= numPartitions; i++ {
		if i > 0 {
			partitionPath = fmt.Sprintf("%s%d", devicePath, i)
		}

		loopDeviceNum, err := t.AttachLoopDevice(partitionSize)
		if err != nil {
			return err
		}

		t.RemoveDevice(partitionPath)

		_, err = t.RunCommand(fmt.Sprintf("sudo mknod %s b 7 %d", partitionPath, loopDeviceNum))
		if err != nil {
			return err
		}
	}
	return nil
}

func (t *TestEnvironment) AttachPartitionedRootDevice(devicePath string, sizeInMB, rootPartitionSizeInMB int) (string, string, error) {
	// Partitioner requires fs backed device
	_, err := t.RunCommand(fmt.Sprintf("sudo mknod %s b 7 99", devicePath))
	if err != nil {
		return "", "", err
	}

	attachDeviceTemplate := `
sudo rm -rf /virtual-root-fs
sudo dd if=/dev/zero of=/virtual-root-fs bs=1M count=%d
sudo losetup %s /virtual-root-fs
`
	attachDeviceScript := fmt.Sprintf(attachDeviceTemplate, sizeInMB, devicePath)
	_, err = t.RunCommand(attachDeviceScript)
	if err != nil {
		return "", "", err
	}

	err = t.AttachDevice(devicePath, sizeInMB, 3)
	if err != nil {
		return "", "", err
	}

	// Create only first partition, agent will partition the rest for ephemeral disk
	partitionTemplate := `
echo ',%d,L,' | sudo sfdisk -uM %s
`
	partitionScript := fmt.Sprintf(partitionTemplate, rootPartitionSizeInMB, devicePath)
	_, err = t.RunCommand(partitionScript)
	if err != nil {
		return "", "", err
	}

	rootLink, err := t.RunCommand("df / | grep /dev/ | cut -d' ' -f1")
	if err != nil {
		return "", "", err
	}

	oldRootDevice, err := t.RunCommand(fmt.Sprintf("readlink -f %s", rootLink))
	if err != nil {
		return "", "", err
	}

	// Agent reads the symlink to get root device
	// Replace the symlink with our fake device
	err = t.SwitchRootDevice(devicePath, rootLink)
	if err != nil {
		return "", "", err
	}

	return strings.TrimSpace(oldRootDevice), strings.TrimSpace(rootLink), nil
}

func (t *TestEnvironment) SwitchRootDevice(devicePath, rootLink string) error {
	_, err := t.RunCommand(fmt.Sprintf("sudo rm -f %s", rootLink))
	if err != nil {
		return err
	}

	_, err = t.RunCommand(fmt.Sprintf("sudo ln -s %s1 %s", devicePath, rootLink))
	if err != nil {
		return err
	}

	return nil
}

func (t *TestEnvironment) DetachDevice(devicePath string) error {
	mountPoint, err := t.RunCommand(fmt.Sprintf("sudo mount | grep %s | cut -d ' ' -f 3", devicePath))
	if err != nil {
		return err
	}

	if mountPoint != "" {
		_, err = t.RunCommand(fmt.Sprintf("sudo umount -l %s", mountPoint))
		if err != nil {
			return err
		}
	}
	_, err = t.RunCommand(fmt.Sprintf("sudo rm -f %s*", devicePath))
	return err
}

func (t *TestEnvironment) RemoveDevice(devicePath string) error {
	_, err := t.RunCommand(fmt.Sprintf("sudo rm -f %s", devicePath))
	return err
}

func (t *TestEnvironment) AttachLoopDevice(size int) (int, error) {
	loopDeviceNum := t.currentDeviceNum
	devicePath := fmt.Sprintf("/dev/loop%d", t.currentDeviceNum)

	t.DetachLoopDevice(devicePath)

	attachDeviceTemplate := `
sudo rm -rf /virtualfs-%d
sudo dd if=/dev/zero of=/virtualfs-%d bs=1M count=%d
sudo losetup %s /virtualfs-%d
`
	attachDeviceScript := fmt.Sprintf(attachDeviceTemplate, loopDeviceNum, loopDeviceNum, size, devicePath, loopDeviceNum)
	_, err := t.RunCommand(attachDeviceScript)
	if err != nil {
		return 0, err
	}

	t.currentDeviceNum++

	return loopDeviceNum, nil
}

func (t *TestEnvironment) DetachLoopDevice(devicePath string) error {
	_, err := t.RunCommand(fmt.Sprintf("sudo losetup -d %s", devicePath))
	return err
}

func (t *TestEnvironment) UpdateAgentConfig(configFile string) error {
	_, err := t.RunCommand(
		fmt.Sprintf(
			"sudo cp %s/%s /var/vcap/bosh/agent.json",
			t.assetsDir(),
			configFile,
		),
	)
	return err
}

func (t *TestEnvironment) RestartAgent() error {
	err := t.StopAgent()
	if err != nil {
		return err
	}

	return t.StartAgent()
}

func (t *TestEnvironment) StopAgent() error {
	_, err := t.RunCommand("nohup sudo sv stop agent &")
	return err
}

func (t *TestEnvironment) StartAgent() error {
	_, err := t.RunCommand("nohup sudo sv start agent &")
	return err
}

func (t *TestEnvironment) StartRegistry(settings boshsettings.Settings) error {
	settingsJSON, err := json.Marshal(settings)
	if err != nil {
		return err
	}

	_, err = t.RunCommand("sudo rm -f /var/vcap/bosh/settings.json")
	if err != nil {
		return err
	}

	t.RunCommand("sudo killall -9 fake-registry")

	_, err = t.RunCommand(
		fmt.Sprintf(
			`nohup %s/tmp/fake-registry -user user -password pass -host 127.0.0.1 -port 9090 -instance instance-id -settings %s &> /dev/null &`,
			t.agentDir(),
			strconv.Quote(string(settingsJSON)),
		),
	)
	return err
}

func (t *TestEnvironment) GetFileContents(filePath string) (string, error) {
	return t.RunCommand(
		fmt.Sprintf(
			`cat %s`,
			filePath,
		),
	)
}

func (t *TestEnvironment) RunCommand(command string) (string, error) {
	stdout, _, _, err := t.cmdRunner.RunCommand(
		"vagrant",
		"ssh",
		"-c",
		command,
	)

	return stdout, err
}

func (t *TestEnvironment) agentDir() string {
	return "/home/vagrant/go/src/github.com/cloudfoundry/bosh-agent"
}

func (t *TestEnvironment) assetsDir() string {
	return fmt.Sprintf("%s/integration/assets", t.agentDir())
}
