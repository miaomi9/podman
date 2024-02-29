//go:build !darwin

package qemu

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/containers/common/pkg/config"
	"github.com/containers/common/pkg/strongunits"
	gvproxy "github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/containers/podman/v5/pkg/machine"
	"github.com/containers/podman/v5/pkg/machine/define"
	"github.com/containers/podman/v5/pkg/machine/ignition"
	"github.com/containers/podman/v5/pkg/machine/qemu/command"
	"github.com/containers/podman/v5/pkg/machine/shim/diskpull"
	"github.com/containers/podman/v5/pkg/machine/sockets"
	"github.com/containers/podman/v5/pkg/machine/vmconfigs"
	"github.com/sirupsen/logrus"
)

type QEMUStubber struct {
	vmconfigs.QEMUConfig
	// Command describes the final QEMU command line
	Command command.QemuCmd
}

var (
	gvProxyWaitBackoff        = 500 * time.Millisecond
	gvProxyMaxBackoffAttempts = 6
)

func (q QEMUStubber) UserModeNetworkEnabled(*vmconfigs.MachineConfig) bool {
	return true
}

func (q QEMUStubber) UseProviderNetworkSetup() bool {
	return false
}

func (q QEMUStubber) RequireExclusiveActive() bool {
	return true
}

func (q *QEMUStubber) setQEMUCommandLine(mc *vmconfigs.MachineConfig) error {
	qemuBinary, err := findQEMUBinary()
	if err != nil {
		return err
	}

	ignitionFile, err := mc.IgnitionFile()
	if err != nil {
		return err
	}

	readySocket, err := mc.ReadySocket()
	if err != nil {
		return err
	}

	q.QEMUPidPath = mc.QEMUHypervisor.QEMUPidPath

	q.Command = command.NewQemuBuilder(qemuBinary, q.addArchOptions(nil))
	q.Command.SetBootableImage(mc.ImagePath.GetPath())
	q.Command.SetMemory(mc.Resources.Memory)
	q.Command.SetCPUs(mc.Resources.CPUs)
	q.Command.SetIgnitionFile(*ignitionFile)
	q.Command.SetQmpMonitor(mc.QEMUHypervisor.QMPMonitor)
	gvProxySock, err := mc.GVProxySocket()
	if err != nil {
		return err
	}
	if err := q.Command.SetNetwork(gvProxySock); err != nil {
		return err
	}
	q.Command.SetSerialPort(*readySocket, *mc.QEMUHypervisor.QEMUPidPath, mc.Name)

	// Add volumes to qemu command line
	for _, mount := range mc.Mounts {
		// the index provided in this case is thrown away
		_, _, _, _, securityModel := vmconfigs.SplitVolume(0, mount.OriginalInput)
		q.Command.SetVirtfsMount(mount.Source, mount.Tag, securityModel, mount.ReadOnly)
	}

	q.Command.SetUSBHostPassthrough(mc.Resources.USBs)

	return nil
}

func (q *QEMUStubber) CreateVM(opts define.CreateVMOpts, mc *vmconfigs.MachineConfig, builder *ignition.IgnitionBuilder) error {
	monitor, err := command.NewQMPMonitor(opts.Name, opts.Dirs.RuntimeDir)
	if err != nil {
		return err
	}

	qemuConfig := vmconfigs.QEMUConfig{
		QMPMonitor: monitor,
	}
	machineRuntimeDir, err := mc.RuntimeDir()
	if err != nil {
		return err
	}

	qemuPidPath, err := machineRuntimeDir.AppendToNewVMFile(mc.Name+"_vm.pid", nil)
	if err != nil {
		return err
	}

	mc.QEMUHypervisor = &qemuConfig
	mc.QEMUHypervisor.QEMUPidPath = qemuPidPath
	return q.resizeDisk(strongunits.GiB(mc.Resources.DiskSize), mc.ImagePath)
}

func runStartVMCommand(cmd *exec.Cmd) error {
	err := cmd.Start()
	if err != nil {
		// check if qemu was not found
		// look up qemu again maybe the path was changed, https://github.com/containers/podman/issues/13394
		cfg, err := config.Default()
		if err != nil {
			return err
		}
		qemuBinaryPath, err := cfg.FindHelperBinary(QemuCommand, true)
		if err != nil {
			return err
		}
		cmd.Path = qemuBinaryPath
		err = cmd.Start()
		if err != nil {
			return fmt.Errorf("unable to execute %q: %w", cmd, err)
		}
	}
	return nil
}

func (q *QEMUStubber) StartVM(mc *vmconfigs.MachineConfig) (func() error, func() error, error) {
	if err := q.setQEMUCommandLine(mc); err != nil {
		return nil, nil, fmt.Errorf("unable to generate qemu command line: %q", err)
	}

	readySocket, err := mc.ReadySocket()
	if err != nil {
		return nil, nil, err
	}

	gvProxySock, err := mc.GVProxySocket()
	if err != nil {
		return nil, nil, err
	}

	// Wait on gvproxy to be running and aware
	if err := sockets.WaitForSocketWithBackoffs(gvProxyMaxBackoffAttempts, gvProxyWaitBackoff, gvProxySock.GetPath(), "gvproxy"); err != nil {
		return nil, nil, err
	}

	dnr, dnw, err := machine.GetDevNullFiles()
	if err != nil {
		return nil, nil, err
	}
	defer dnr.Close()
	defer dnw.Close()

	cmdLine := q.Command

	// Disable graphic window when not in debug mode
	// Done in start, so we're not suck with the debug level we used on init
	if !logrus.IsLevelEnabled(logrus.DebugLevel) {
		cmdLine.SetDisplay("none")
	}

	logrus.Debugf("qemu cmd: %v", cmdLine)

	stderrBuf := &bytes.Buffer{}

	// actually run the command that starts the virtual machine
	cmd := &exec.Cmd{
		Args:   cmdLine,
		Path:   cmdLine[0],
		Stdin:  dnr,
		Stdout: dnw,
		Stderr: stderrBuf,
	}

	if err := runStartVMCommand(cmd); err != nil {
		return nil, nil, err
	}
	logrus.Debugf("Started qemu pid %d", cmd.Process.Pid)

	readyFunc := func() error {
		return waitForReady(readySocket, cmd.Process.Pid, stderrBuf)
	}

	// if this is not the last line in the func, make it a defer
	return cmd.Process.Release, readyFunc, nil
}

func waitForReady(readySocket *define.VMFile, pid int, stdErrBuffer *bytes.Buffer) error {
	defaultBackoff := 500 * time.Millisecond
	maxBackoffs := 6
	conn, err := sockets.DialSocketWithBackoffsAndProcCheck(maxBackoffs, defaultBackoff, readySocket.GetPath(), checkProcessStatus, "qemu", pid, stdErrBuffer)
	if err != nil {
		return err
	}
	defer conn.Close()

	_, err = bufio.NewReader(conn).ReadString('\n')
	return err
}

func (q *QEMUStubber) Exists(name string) (bool, error) {
	return false, nil
}

func (q *QEMUStubber) VMType() define.VMType {
	return define.QemuVirt
}

func (q *QEMUStubber) PrepareIgnition(_ *vmconfigs.MachineConfig, _ *ignition.IgnitionBuilder) (*ignition.ReadyUnitOpts, error) {
	return nil, nil
}

func (q *QEMUStubber) StopHostNetworking(_ *vmconfigs.MachineConfig, _ define.VMType) error {
	return define.ErrNotImplemented
}

func (q *QEMUStubber) resizeDisk(newSize strongunits.GiB, diskPath *define.VMFile) error {
	// Find the qemu executable
	cfg, err := config.Default()
	if err != nil {
		return err
	}
	resizePath, err := cfg.FindHelperBinary("qemu-img", true)
	if err != nil {
		return err
	}
	resize := exec.Command(resizePath, []string{"resize", diskPath.GetPath(), strconv.Itoa(int(newSize)) + "G"}...)
	resize.Stdout = os.Stdout
	resize.Stderr = os.Stderr
	if err := resize.Run(); err != nil {
		return fmt.Errorf("resizing image: %q", err)
	}

	return nil
}

func (q *QEMUStubber) SetProviderAttrs(mc *vmconfigs.MachineConfig, opts define.SetOptions) error {
	state, err := q.State(mc, false)
	if err != nil {
		return err
	}
	if state != define.Stopped {
		return errors.New("unable to change settings unless vm is stopped")
	}

	if opts.DiskSize != nil {
		if err := q.resizeDisk(*opts.DiskSize, mc.ImagePath); err != nil {
			return err
		}
	}

	if opts.Rootful != nil && mc.HostUser.Rootful != *opts.Rootful {
		if err := mc.SetRootful(*opts.Rootful); err != nil {
			return err
		}
	}

	if opts.USBs != nil {
		usbs, err := define.ParseUSBs(*opts.USBs)
		if err != nil {
			return err
		}
		mc.Resources.USBs = usbs
	}

	// Because QEMU does nothing with these hardware attributes, we can simply return
	return nil
}

func (q *QEMUStubber) StartNetworking(mc *vmconfigs.MachineConfig, cmd *gvproxy.GvproxyCommand) error {
	gvProxySock, err := mc.GVProxySocket()
	if err != nil {
		return err
	}
	// make sure it does not exist before gvproxy is called
	if err := gvProxySock.Delete(); err != nil {
		logrus.Error(err)
	}
	cmd.AddQemuSocket(fmt.Sprintf("unix://%s", filepath.ToSlash(gvProxySock.GetPath())))
	return nil
}

func (q *QEMUStubber) RemoveAndCleanMachines(_ *define.MachineDirs) error {
	// nothing to do but remove files
	return nil
}

// mountVolumesToVM iterates through the machine's volumes and mounts them to the
// machine
// TODO this should probably be temporary; mount code should probably be its own package and shared completely
func (q *QEMUStubber) MountVolumesToVM(mc *vmconfigs.MachineConfig, quiet bool) error {
	for _, mount := range mc.Mounts {
		if !quiet {
			fmt.Printf("Mounting volume... %s:%s\n", mount.Source, mount.Target)
		}
		// create mountpoint directory if it doesn't exist
		// because / is immutable, we have to monkey around with permissions
		// if we dont mount in /home or /mnt
		args := []string{"-q", "--"}
		if !strings.HasPrefix(mount.Target, "/home") && !strings.HasPrefix(mount.Target, "/mnt") {
			args = append(args, "sudo", "chattr", "-i", "/", ";")
		}
		args = append(args, "sudo", "mkdir", "-p", mount.Target)
		if !strings.HasPrefix(mount.Target, "/home") && !strings.HasPrefix(mount.Target, "/mnt") {
			args = append(args, ";", "sudo", "chattr", "+i", "/", ";")
		}
		err := machine.CommonSSH(mc.SSH.RemoteUsername, mc.SSH.IdentityPath, mc.Name, mc.SSH.Port, args)
		if err != nil {
			return err
		}
		switch mount.Type {
		case MountType9p:
			mountOptions := []string{"-t", "9p"}
			mountOptions = append(mountOptions, []string{"-o", "trans=virtio", mount.Tag, mount.Target}...)
			mountOptions = append(mountOptions, []string{"-o", "version=9p2000.L,msize=131072,cache=mmap"}...)
			if mount.ReadOnly {
				mountOptions = append(mountOptions, []string{"-o", "ro"}...)
			}
			err = machine.CommonSSH(mc.SSH.RemoteUsername, mc.SSH.IdentityPath, mc.Name, mc.SSH.Port, append([]string{"-q", "--", "sudo", "mount"}, mountOptions...))
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown mount type: %s", mount.Type)
		}
	}
	return nil
}

func (q *QEMUStubber) MountType() vmconfigs.VolumeMountType {
	return vmconfigs.NineP
}

func (q *QEMUStubber) PostStartNetworking(mc *vmconfigs.MachineConfig, noInfo bool) error {
	return nil
}

func (q *QEMUStubber) GetDisk(userInputPath string, dirs *define.MachineDirs, mc *vmconfigs.MachineConfig) error {
	return diskpull.GetDisk(userInputPath, dirs, mc.ImagePath, q.VMType(), mc.Name)
}
