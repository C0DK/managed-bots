package base

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kballard/go-shellquote"
	"github.com/keybase/go-keybase-chat-bot/kbchat"
	"github.com/keybase/go-keybase-chat-bot/kbchat/types/chat1"
	"golang.org/x/sync/errgroup"
)

type Handler interface {
	HandleCommand(chat1.MsgSummary) error
	HandleNewConv(chat1.ConvSummary) error
}

type Shutdowner interface {
	Shutdown() error
}

type Server struct {
	*DebugOutput
	sync.Mutex

	shutdownCh chan struct{}

	announcement string
	awsOpts      *AWSOptions
	kbc          *kbchat.API
	botAdmins    []string
}

func NewServer(announcement string, awsOpts *AWSOptions) *Server {
	return &Server{
		announcement: announcement,
		awsOpts:      awsOpts,
		botAdmins:    DefaultBotAdmins,
		shutdownCh:   make(chan struct{}),
	}
}

func (s *Server) SetBotAdmins(admins []string) {
	s.botAdmins = admins
}

func (s *Server) Shutdown() error {
	s.Lock()
	defer s.Unlock()
	if s.shutdownCh != nil {
		close(s.shutdownCh)
		s.shutdownCh = nil
		if err := s.kbc.Shutdown(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) HandleSignals(shutdowner Shutdowner) (err error) {
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, os.Interrupt, os.Signal(syscall.SIGTERM))
	sig := <-signalCh
	s.Debug("Received %q, shutting down", sig)
	if err := s.Shutdown(); err != nil {
		s.Debug("Unable to shutdown server: %v", err)
	}
	if shutdowner != nil {
		if err := shutdowner.Shutdown(); err != nil {
			s.Debug("Unable to shutdown shutdowner: %v", err)
		}
	}
	return nil
}

func (s *Server) Start(keybaseLoc, home string) (kbc *kbchat.API, err error) {
	if s.kbc, err = kbchat.Start(kbchat.RunOptions{
		KeybaseLocation: keybaseLoc,
		HomeDir:         home,
	}); err != nil {
		return s.kbc, err
	}
	s.DebugOutput = NewDebugOutput("Server", s.kbc)
	return s.kbc, nil
}

func (s *Server) SendAnnouncement(announcement, running string) (err error) {
	if s.announcement == "" {
		return nil
	}
	defer func() {
		if err == nil {
			s.Debug("announcement success")
		}
	}()
	if _, err := s.kbc.SendMessageByConvID(chat1.ConvIDStr(announcement), running); err == nil {
		return nil
	}
	s.Debug("failed to announce self as conv ID: %s", err)
	if _, err := s.kbc.SendMessageByTlfName(announcement, running); err == nil {
		return nil
	}
	s.Debug("failed to announce self as user: %s", err)
	if _, err := s.kbc.SendMessageByTeamName(announcement, nil, running); err != nil {
		s.Debug("failed to announce self as team: %s", err)
		return err
	}
	return nil
}

func (s *Server) Listen(handler Handler) error {
	sub, err := s.kbc.Listen(kbchat.ListenOptions{Convs: true})
	if err != nil {
		s.Debug("Listen: failed to listen: %s", err)
		return err
	}
	s.Debug("startup success, listening for messages and convs...")
	var eg errgroup.Group
	eg.Go(func() error { return s.listenForMsgs(s.shutdownCh, sub, handler) })
	eg.Go(func() error { return s.listenForConvs(s.shutdownCh, sub, handler) })
	if err := eg.Wait(); err != nil {
		s.Debug("wait error: %s", err)
		return err
	}
	s.Debug("Listen: shut down")
	return nil
}

func (s *Server) listenForMsgs(shutdownCh chan struct{}, sub *kbchat.NewSubscription, handler Handler) error {
	for {
		select {
		case <-shutdownCh:
			s.Debug("listenForMsgs: shutting down")
			return nil
		default:
		}

		m, err := sub.Read()
		if err != nil {
			s.Debug("listenForMsgs: Read() error: %s", err)
			continue
		}

		msg := m.Message
		if msg.Content.Text != nil {
			cmd := strings.TrimSpace(msg.Content.Text.Body)
			switch {
			case strings.HasPrefix(cmd, "!logsend"):
				if err := s.handleLogSend(msg); err != nil {
					s.ChatDebugFull(msg.ConvID, "listenForMsgs: unable to handleLogSend: %v", err)
				}
				continue
			case strings.HasPrefix(cmd, "!botlog"):
				if err := s.handleBotLogs(msg); err != nil {
					s.ChatDebugFull(msg.ConvID, "listenForMsgs: unable to handleBotLogs: %v", err)
				}
				continue
			case strings.HasPrefix(cmd, "!pprof"):
				if err := s.handlePProf(msg); err != nil {
					s.ChatDebugFull(msg.ConvID, "listenForMsgs: unable to handlePProf: %v", err)
				}
				continue
			}
		}

		if err := handler.HandleCommand(msg); err != nil {
			s.ChatDebug(msg.ConvID, "listenForMsgs: unable to HandleCommand: %v", err)
		}
	}
}

func (s *Server) listenForConvs(shutdownCh chan struct{}, sub *kbchat.NewSubscription, handler Handler) error {
	for {
		select {
		case <-shutdownCh:
			s.Debug("listenForConvs: shutting down")
			return nil
		default:
		}

		c, err := sub.ReadNewConvs()
		if err != nil {
			s.Debug("listenForConvs: ReadNewConvs() error: %s", err)
			continue
		}

		if err := handler.HandleNewConv(c.Conversation); err != nil {
			s.Debug("listenForConvs: unable to HandleNewConv: %v", err)
		}
	}
}

func (s *Server) allowHiddenCommand(msg chat1.MsgSummary) bool {
	for _, username := range s.botAdmins {
		if username == msg.Sender.Username {
			return true
		}
	}
	return false
}

func (s *Server) handleLogSend(msg chat1.MsgSummary) error {
	if !s.allowHiddenCommand(msg) {
		s.Debug("ignoring log send from @%s, botAdmins: %v",
			msg.Sender.Username, s.botAdmins)
		return nil
	}

	s.ChatEcho(msg.ConvID, "starting a log send...")
	cmd := s.kbc.Command("log", "send", "--no-confirm", "--feedback",
		fmt.Sprintf("managed-bot log requested by @%s", msg.Sender.Username))
	output, err := cmd.StdoutPipe()
	if err != nil {
		s.ChatDebugFull(msg.ConvID, "unable to get output pipe: %v", err)
		return err
	}
	if err := cmd.Start(); err != nil {
		s.ChatDebugFull(msg.ConvID, "unable to start command: %v", err)
		return err
	}
	outputBytes, err := ioutil.ReadAll(output)
	if err != nil {
		s.ChatDebugFull(msg.ConvID, "unable to read ouput: %v", err)
		return err
	}
	if len(outputBytes) > 0 {
		s.ChatDebugFull(msg.ConvID, "log send output: ```%v```", string(outputBytes))
	}
	if err := cmd.Wait(); err != nil {
		s.ChatDebugFull(msg.ConvID, "unable to finish command: %v", err)
		return err
	}
	return nil
}

func (s *Server) handlePProf(msg chat1.MsgSummary) error {
	if !s.allowHiddenCommand(msg) {
		s.Debug("ignoring pprof from @%s, botAdmins: %v",
			msg.Sender.Username, s.botAdmins)
		return nil
	}

	toks, err := shellquote.Split(msg.Content.Text.Body)
	if err != nil {
		return err
	}
	if len(toks) <= 1 {
		s.ChatDebugFull(msg.ConvID, "must specify 'trace', 'cpu' or 'heap'. Try `!pprof cpu -d 5m`")
		return nil
	}
	// drop `!` from `!pprof`
	toks[0] = strings.TrimPrefix(toks[0], "!")
	dur, err := time.ParseDuration(toks[len(toks)-1])
	if err != nil {
		s.ChatDebugFull(msg.ConvID, "unable to parse duration using default of 5m: %v", err)
		dur = time.Minute * 5
		toks[len(toks)-1] = dur.String()
	}
	outfile := fmt.Sprintf("/tmp/%s-%d.out", toks[1], time.Now().Unix())
	toks = append(toks, outfile)

	s.ChatEcho(msg.ConvID, "starting pprof... %s", toks)
	cmd := s.kbc.Command(toks...)
	if err := cmd.Run(); err != nil {
		s.ChatDebugFull(msg.ConvID, "unable to get run command: %v", err)
		return err
	}
	go func() {
		time.Sleep(dur + time.Second)
		defer func() {
			// Cleanup after the file is sent.
			time.Sleep(time.Minute)
			s.Debug("cleaning up %s", outfile)
			if err = os.Remove(outfile); err != nil {
				s.Debug("unable to clean up %s: %v", outfile, err)
			}
		}()
		if _, err := s.kbc.SendAttachmentByConvID(msg.ConvID, outfile, ""); err != nil {
			s.ChatDebugFull(msg.ConvID, "unable to send attachment profile: %v", err)
		}
	}()
	return nil
}

func (s *Server) handleBotLogs(msg chat1.MsgSummary) error {
	if !s.allowHiddenCommand(msg) {
		s.Debug("ignoring bot log request from @%s, botAdmins: %v",
			msg.Sender.Username, s.botAdmins)
		return nil
	}

	if s.awsOpts == nil {
		return fmt.Errorf("AWS not properly configured")
	}

	s.ChatEcho(msg.ConvID, "fetching logs from cloud watch")
	logs, err := GetLatestCloudwatchLogs(s.awsOpts.AWSRegion, s.awsOpts.CloudWatchLogGroup)
	if err != nil {
		return err
	}
	tld := "private"
	if msg.Channel.MembersType == "team" {
		tld = "team"
	}

	folder := fmt.Sprintf("/keybase/%s/%s/botlogs", tld, msg.Channel.Name)
	if err := exec.Command("keybase", "fs", "mkdir", folder).Run(); err != nil {
		return fmt.Errorf("kbfsOutput: failed to make directory: %s", err)
	}
	fileName := fmt.Sprintf("botlogs-%d.txt", time.Now().Unix())
	filePath := fmt.Sprintf("/tmp/%s", fileName)
	defer os.Remove(filePath)
	if err := ioutil.WriteFile(filePath, []byte(strings.Join(logs, "\n")), 0644); err != nil {
		return fmt.Errorf("kbfsOutput: failed to write log output: %s", err)
	}
	if err := exec.Command("keybase", "fs", "mv", filePath, folder).Run(); err != nil {
		return fmt.Errorf("kbfsOutput: failed to move log output: %s", err)
	}
	destFilePath := fmt.Sprintf("%s/%s", folder, fileName)
	s.ChatEcho(msg.ConvID, "log output: %s", destFilePath)
	return nil
}
