package service

import (
	"github.com/project-nano/framework"
	"net/http"
	"net"
	"github.com/julienschmidt/httprouter"
	"log"
	"strings"
	"fmt"
	"context"
	"errors"
	"net/textproto"
	"mime/multipart"
	"math/rand"
	"time"
	"github.com/amoghe/go-crypt"
)

type GuestInitiator struct {
	listener            net.Listener
	listenAddress       string
	listenDevice        string
	server              http.Server
	eventChan           chan InstanceStatusChangedEvent
	insManager          *InstanceManager
	supportedInterfaces []string
	generator           *rand.Rand
	runner              *framework.SimpleRunner
}

const (
	InitiatorMagicPort = 25469
	ListenerName       = "initiator"
)

func CreateInitiator(netDev string, manager *InstanceManager) (initiator *GuestInitiator, err error) {
	const (
		DefaultQueueSize = 1 << 10
	)
	magicIP, err := GetCurrentIPOfDefaultBridge()
	if err != nil{
		log.Printf("<initiator> get default ip fail: %s", err.Error())
		return
	}
	initiator = &GuestInitiator{}
	initiator.listenDevice = netDev
	initiator.listenAddress = fmt.Sprintf("%s:%d", magicIP, InitiatorMagicPort)
	initiator.generator = rand.New(rand.NewSource(time.Now().UnixNano()))
	if err = initiator.listenMagicAddress(); err != nil {
		return
	}
	if err = initiator.prepareServer();err != nil{
		return
	}
	initiator.eventChan = make(chan InstanceStatusChangedEvent, DefaultQueueSize)
	initiator.insManager = manager
	initiator.runner = framework.CreateSimpleRunner(initiator.Routine)
	return initiator, nil
}

func (initiator *GuestInitiator) listenMagicAddress() (err error) {
	const(
		Protocol = "tcp"
	)
	initiator.listener, err = net.Listen(Protocol, initiator.listenAddress)
	if err != nil{
		return err
	}
	log.Printf("<initiator> listen at %s success", initiator.listenAddress)
	return nil
}

func (initiator *GuestInitiator) prepareServer() (err error) {
	var router = httprouter.New()
	var noHandler = NotFoundHandler{}
	router.NotFound = &noHandler

	router.GET("/:version/:id/meta-data", initiator.getMetaData)
	router.GET("/:version/:id/user-data", initiator.getUserData)

	initiator.server.Addr = initiator.listenAddress
	initiator.server.Handler = router

	initiator.supportedInterfaces = []string{"hostname", "instance-id", "local-hostname", "local-ipv4", "public-ipv4"}
	return nil
}

func (initiator *GuestInitiator) Start() error{
	return initiator.runner.Start()
}

func (initiator *GuestInitiator) Stop() error{
	return initiator.runner.Stop()
}

func (initiator *GuestInitiator) Routine(c framework.RoutineController) {
	initiator.insManager.AddEventListener(ListenerName, initiator.eventChan)
	go initiator.serveCloudInit()
	for !c.IsStopping(){
		select {
		case <- c.GetNotifyChannel():
			c.SetStopping()
			var ctx = context.TODO()
			var err = initiator.server.Shutdown(ctx)
			if err != nil{
				log.Printf("<initiator> shutdown http server: %s", err.Error())
			}
		case event := <- initiator.eventChan:
			initiator.handleGuestEvent(event)
		}
	}
	initiator.insManager.RemoveEventListener(ListenerName)
	//initiator.removeMagicAddress(initiator.listenDevice, initiator.magicNetwork)
	c.NotifyExit()
}

func (initiator *GuestInitiator) serveCloudInit(){
	log.Println("<initiator> http server started")
	var err = initiator.server.Serve(initiator.listener)
	if err != nil{
		log.Printf("<initiator> http server finished: %s", err.Error())
	}
}

func (initiator *GuestInitiator) getMetaData(w http.ResponseWriter, r *http.Request, params httprouter.Params){
	var version = params.ByName("version")
	var guestID = params.ByName("id")

	log.Printf("<initiator> query metadata from %s, version %s", r.RemoteAddr, version)
	var respChan = make(chan InstanceResult, 1)
	initiator.insManager.GetInstanceConfig(guestID, respChan)
	var result = <- respChan
	if result.Error != nil{
		log.Printf("<initiator> get meta data for guest '%s' fail: %s", guestID, result.Error.Error())
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(result.Error.Error()))
		return
	}

	w.WriteHeader(http.StatusOK)
	var ins = result.Instance
	fmt.Fprintf(w, "instance-id: %s\n", ins.ID)
	var hostname = strings.TrimPrefix(ins.Name, fmt.Sprintf("%s.", ins.Group))
	fmt.Fprintf(w, "hostname: %s\n", hostname)
}

func (initiator *GuestInitiator) getUserData(w http.ResponseWriter, r *http.Request, params httprouter.Params)  {
	var version = params.ByName("version")
	var guestID = params.ByName("id")

	log.Printf("<initiator> query user data from %s, version %s", r.RemoteAddr, version)
	var respChan = make(chan InstanceResult, 1)
	initiator.insManager.GetInstanceConfig(guestID, respChan)
	var result = <- respChan
	if result.Error != nil{
		log.Printf("<initiator> get user data for guest '%s' fail: %s", guestID, result.Error.Error())
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(result.Error.Error()))
		return
	}
	//todo: modified flag (password/disks)
	var guest = result.Instance
	if !guest.Initialized{
		data, err := initiator.buildInitialConfig(guest.GuestConfig)
		if err != nil{
			log.Printf("<initiator> build config for guest '%s' fail: %s", guestID, err.Error())
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(result.Error.Error()))
			return
		}

		var partHeader = make(textproto.MIMEHeader)
		partHeader.Add("Content-Type", "text/cloud-config")
		partHeader.Add("Content-Disposition", "attachment; filename=\"cloud-config.txt\"")
		partHeader.Add("MIME-Version", "1.0")
		partHeader.Add("Content-Transfer-Encoding", "7bit")

		var multiWriter = multipart.NewWriter(w)
		w.Write([]byte(fmt.Sprintf("Content-Type: multipart/mixed; boundary=\"%s\"\n", multiWriter.Boundary())))
		w.Write([]byte("MIME-Version: 1.0\n\n"))

		defer multiWriter.Close()
		partWriter, err := multiWriter.CreatePart(partHeader)
		if err != nil{
			log.Printf("<initiator> build config part for guest '%s' fail: %s", guestID, err.Error())
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(result.Error.Error()))
			return
		}

		partWriter.Write([]byte(data))
		var respChan = make(chan error, 1)
		initiator.insManager.FinishGuestInitialize(guestID, respChan)
		if err = <- respChan; err != nil{
			log.Printf("<initiator> warning: finish guest '%s' initialize fail: %s", guestID, err.Error())
		}else{
			log.Printf("<initiator> guest '%s' initialized", guestID)
		}
	}


}

func (initiator *GuestInitiator) handleGuestEvent(event InstanceStatusChangedEvent){

}

type NotFoundHandler struct {

}

func (handler *NotFoundHandler) ServeHTTP(w http.ResponseWriter, r *http.Request)  {
	log.Printf("<initiator> ignore %s %s from %s", r.Method, r.URL.RawQuery, r.RemoteAddr)
}

func (initiator *GuestInitiator)buildInitialConfig(config GuestConfig) (data string, err error) {
	var os = config.Template.OperatingSystem
	switch os {
	case SystemNameLinux:
		return initiator.buildLinuxInitialization(config)
	default:
		err = fmt.Errorf("unsupported oprating system '%s'", os)
		return "", err
	}
}

func (initiator *GuestInitiator) buildLinuxInitialization(config GuestConfig) (data string, err error) {
	const (
		AdminGroup            = "wheel"
		StartDeviceCharacter  = 0x61 //'a'
		DevicePrefixSCSI      = "sd"
		VolumeGroupName       = "nano"
		DataLogicalVolumeName = "data"
		SystemPartitionIndex  = 2
		RootVolume            = "/dev/centos/root"
		SaltLength            = 8
	)
	var builder strings.Builder
	builder.WriteString("#cloud-config\n")
	if config.RootLoginEnabled{
		builder.WriteString("disable_root: false\n")
	}else{
		builder.WriteString("disable_root: true\n")
	}


	builder.WriteString("ssh_pwauth: yes\n")
	if config.AuthUser == AdminLinux{
		//change default password
		fmt.Fprintf(&builder, "chpasswd:\n  expire: false\n  list: |\n    %s:%s\n\n", config.AuthUser, config.AuthSecret)
	}else{
		//new admin
		var salt = initiator.generateSalt(SaltLength)

		hashed, err := crypt.Crypt(config.AuthSecret, fmt.Sprintf("$6$%s$", salt))
		if err != nil{
			return data, err
		}
		fmt.Fprintf(&builder, "users:\n  - name: %s\n    passwd: %s\n    lock_passwd: false\n    groups: [ %s ]\n\n", config.AuthUser, hashed, AdminGroup)
	}

	builder.WriteString("bootcmd:\n")
	var hostname = strings.TrimPrefix(config.Name, fmt.Sprintf("%s.", config.Group))
	fmt.Fprintf(&builder, "    - [ hostnamectl, set-hostname, %s ]\n", hostname)

	var mountMap = map[string]string{}
	var groupDevices []string
	if len(config.Disks) > 1{
		//data disk available
		for i, _ := range config.Disks[1:]{
			var devName = fmt.Sprintf("/dev/%s%c", DevicePrefixSCSI, StartDeviceCharacter + i + 1)//from /dev/sdb
			groupDevices = append(groupDevices, devName)
			fmt.Fprintf(&builder, "    - [ pvcreate, %s ]\n", devName)
		}
		fmt.Fprintf(&builder, "    - [ vgcreate, %s , %s ]\n", VolumeGroupName, strings.Join(groupDevices, ","))
		//lvcreate --name data -l 100%FREE data
		fmt.Fprintf(&builder, "    - [ lvcreate, --name, %s, -l, 100%%FREE, %s ]\n", DataLogicalVolumeName, VolumeGroupName)
		var dataVolume = fmt.Sprintf("/dev/%s/%s", VolumeGroupName, DataLogicalVolumeName)
		fmt.Fprintf(&builder, "    - [ mkfs.ext4, %s ]\n\n", dataVolume)
		if "" == config.DataPath{
			err = errors.New("must specify mount data path in guest")
			return
		}
		mountMap[dataVolume] = config.DataPath
	}
	if 0 != len(mountMap){
		builder.WriteString("mounts:\n")
		for dev, path := range mountMap{
			fmt.Fprintf(&builder, "    - [ %s, %s ]\n", dev, path)
		}
		builder.WriteString("\n")
	}
	var systemDev = fmt.Sprintf("/dev/%s%c%d", DevicePrefixSCSI, StartDeviceCharacter, SystemPartitionIndex) // /dev/sda2
	fmt.Fprintf(&builder, "growpart:\n  mode: auto\n  devices: ['%s']\n  ignore_growroot_disabled: false\n", systemDev)
	fmt.Fprintf(&builder, "runcmd:\n  - [ pvresize, '%s']\n  - [ lvextend, '-l', '+100%%FREE', '%s']\n  - [ xfs_growfs, '%s' ]\n\n",
		systemDev, RootVolume, RootVolume)
	return builder.String(), nil
}

func (initiator *GuestInitiator) generateSalt(length int) (salt string){
	const (
		CharSet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	)
	var result = make([]byte, length)
	var n = len(CharSet)
	for i := 0 ; i < length; i++{
		result[i] = CharSet[initiator.generator.Intn(n)]
	}
	return string(result)
}