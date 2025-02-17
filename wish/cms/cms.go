package cms

import (
	"errors"
	"fmt"
	"math"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/ssh"
	bm "github.com/charmbracelet/wish/bubbletea"
	"github.com/muesli/reflow/indent"
	"github.com/muesli/reflow/wordwrap"
	"github.com/muesli/reflow/wrap"
	"github.com/picosh/pico/db"
	"github.com/picosh/pico/db/postgres"
	"github.com/picosh/pico/shared/storage"
	"github.com/picosh/pico/wish/cms/config"
	"github.com/picosh/pico/wish/cms/ui/account"
	"github.com/picosh/pico/wish/cms/ui/common"
	"github.com/picosh/pico/wish/cms/ui/info"
	"github.com/picosh/pico/wish/cms/ui/keys"
	"github.com/picosh/pico/wish/cms/ui/posts"
	"github.com/picosh/pico/wish/cms/ui/tokens"
	"github.com/picosh/pico/wish/cms/ui/username"
	"github.com/picosh/pico/wish/cms/util"
)

type status int

const (
	statusInit status = iota
	statusReady
	statusNoAccount
	statusBrowsingPosts
	statusBrowsingKeys
	statusBrowsingTokens
	statusSettingUsername
	statusQuitting
)

func (s status) String() string {
	return [...]string{
		"initializing",
		"ready",
		"setting username",
		"browsing posts",
		"browsing keys",
		"quitting",
		"error",
	}[s]
}

// menuChoice represents a chosen menu item.
type menuChoice int

// menu choices.
const (
	setUserChoice menuChoice = iota
	postsChoice
	keysChoice
	tokensChoice
	exitChoice
	unsetChoice // set when no choice has been made
)

// menu text corresponding to menu choices. these are presented to the user.
var menuChoices = map[menuChoice]string{
	setUserChoice: "Set username",
	keysChoice:    "Manage keys",
	tokensChoice:  "Manage tokens",
	postsChoice:   "Manage posts",
	exitChoice:    "Exit",
}

var (
	spinnerStyle = lipgloss.NewStyle().
		Foreground(lipgloss.AdaptiveColor{Light: "#8E8E8E", Dark: "#747373"})
)

func NewSpinner() spinner.Model {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = spinnerStyle
	return s
}

type GotDBMsg db.DB

func Middleware(cfg *config.ConfigCms, urls config.ConfigURL) bm.Handler {
	return func(s ssh.Session) (tea.Model, []tea.ProgramOption) {
		logger := cfg.Logger

		_, _, active := s.Pty()
		if !active {
			logger.Info("no active terminal, skipping")
			return nil, nil
		}
		key, err := util.KeyText(s)
		if err != nil {
			logger.Error(err)
		}

		sshUser := s.User()

		dbpool := postgres.NewDB(cfg.DbURL, cfg.Logger)

		var st storage.ObjectStorage
		if cfg.MinioURL == "" {
			st, err = storage.NewStorageFS(cfg.StorageDir)
		} else {
			st, err = storage.NewStorageMinio(cfg.MinioURL, cfg.MinioUser, cfg.MinioPass)
		}

		if err != nil {
			logger.Fatal(err)
		}

		m := model{
			cfg:        cfg,
			urls:       urls,
			publicKey:  key,
			dbpool:     dbpool,
			st:         st,
			sshUser:    sshUser,
			status:     statusInit,
			menuChoice: unsetChoice,
			styles:     common.DefaultStyles(),
			spinner:    common.NewSpinner(),
			terminalSize: tea.WindowSizeMsg{
				Width:  80,
				Height: 24,
			},
		}

		user, err := m.findUser()
		if err != nil {
			_, _ = fmt.Fprintln(s.Stderr(), err)
			return nil, nil
		}
		m.user = user

		return m, []tea.ProgramOption{tea.WithAltScreen()}
	}
}

// Just a generic tea.Model to demo terminal information of ssh.
type model struct {
	cfg           *config.ConfigCms
	urls          config.ConfigURL
	publicKey     string
	dbpool        db.DB
	st            storage.ObjectStorage
	user          *db.User
	err           error
	sshUser       string
	status        status
	menuIndex     int
	menuChoice    menuChoice
	terminalSize  tea.WindowSizeMsg
	styles        common.Styles
	info          info.Model
	spinner       spinner.Model
	username      username.Model
	posts         posts.Model
	keys          keys.Model
	tokens        tokens.Model
	createAccount account.CreateModel
}

func (m model) Init() tea.Cmd {
	return m.spinner.Tick
}

func (m model) findUser() (*db.User, error) {
	logger := m.cfg.Logger
	var user *db.User

	if m.sshUser == "new" {
		logger.Infof("User requesting to register account")
		return nil, nil
	}

	user, err := m.dbpool.FindUserForKey(m.sshUser, m.publicKey)

	if err != nil {
		logger.Error(err)
		// we only want to throw an error for specific cases
		if errors.Is(err, &db.ErrMultiplePublicKeys{}) {
			return nil, err
		}
		return nil, nil
	}

	return user, nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var (
		cmds []tea.Cmd
		cmd  tea.Cmd
	)

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.terminalSize = msg
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC:
			m.dbpool.Close()
			return m, tea.Quit
		}

		if m.status == statusReady { // Process keys for the menu
			switch msg.String() {
			// Quit
			case "q", "esc":
				m.status = statusQuitting
				m.dbpool.Close()
				return m, tea.Quit

			// Prev menu item
			case "up", "k":
				m.menuIndex--
				if m.menuIndex < 0 {
					m.menuIndex = len(menuChoices) - 1
				}

			// Select menu item
			case "enter":
				m.menuChoice = menuChoice(m.menuIndex)

			// Next menu item
			case "down", "j":
				m.menuIndex++
				if m.menuIndex >= len(menuChoices) {
					m.menuIndex = 0
				}
			}
		}
	case username.NameSetMsg:
		m.status = statusReady
		m.info.User.Name = string(msg)
		m.user = m.info.User
		m.username = username.NewModel(m.dbpool, m.user, m.sshUser) // reset the state
	case account.CreateAccountMsg:
		m.status = statusReady
		m.info.User = msg
		m.user = msg
		m.username = username.NewModel(m.dbpool, m.user, m.sshUser)
		m.info = info.NewModel(m.cfg, m.urls, m.user)
		m.keys = keys.NewModel(m.cfg, m.dbpool, m.user)
		m.tokens = tokens.NewModel(m.cfg, m.dbpool, m.user)
		m.createAccount = account.NewCreateModel(m.cfg, m.dbpool, m.publicKey)

		perPage := math.Floor(float64(m.terminalSize.Height) / 10.0)
		m.posts = posts.NewModel(m.cfg, m.urls, m.dbpool, m.user, m.st, int(perPage))
	}

	switch m.status {
	case statusInit:
		m.username = username.NewModel(m.dbpool, m.user, m.sshUser)
		m.info = info.NewModel(m.cfg, m.urls, m.user)
		m.keys = keys.NewModel(m.cfg, m.dbpool, m.user)
		m.tokens = tokens.NewModel(m.cfg, m.dbpool, m.user)
		m.createAccount = account.NewCreateModel(m.cfg, m.dbpool, m.publicKey)
		if m.user == nil {
			m.status = statusNoAccount
		} else {
			m.status = statusReady
		}

		perPage := math.Floor(float64(m.terminalSize.Height) / 10.0)
		m.posts = posts.NewModel(m.cfg, m.urls, m.dbpool, m.user, m.st, int(perPage))
	}

	m, cmd = updateChildren(msg, m)
	if cmd != nil {
		cmds = append(cmds, cmd)
	}

	return m, tea.Batch(cmds...)
}

func updateChildren(msg tea.Msg, m model) (model, tea.Cmd) {
	var cmd tea.Cmd

	switch m.status {
	case statusBrowsingPosts:
		newModel, newCmd := m.posts.Update(msg)
		postsModel, ok := newModel.(posts.Model)
		if !ok {
			panic("could not perform assertion on posts model")
		}
		m.posts = postsModel
		cmd = newCmd

		if m.posts.Exit {
			perPage := math.Floor(float64(m.terminalSize.Height) / 10.0)
			m.posts = posts.NewModel(m.cfg, m.urls, m.dbpool, m.user, m.st, int(perPage))

			m.posts = posts.NewModel(m.cfg, m.urls, m.dbpool, m.user, m.st, int(perPage))
			m.status = statusReady
		} else if m.posts.Quit {
			m.status = statusQuitting
			return m, tea.Quit
		}
	case statusBrowsingKeys:
		newModel, newCmd := m.keys.Update(msg)
		keysModel, ok := newModel.(keys.Model)
		if !ok {
			panic("could not perform assertion on posts model")
		}
		m.keys = keysModel
		cmd = newCmd

		if m.keys.Exit {
			m.keys = keys.NewModel(m.cfg, m.dbpool, m.user)
			m.status = statusReady
		} else if m.keys.Quit {
			m.status = statusQuitting
			return m, tea.Quit
		}
	case statusBrowsingTokens:
		newModel, newCmd := m.tokens.Update(msg)
		tokensModel, ok := newModel.(tokens.Model)
		if !ok {
			panic("could not perform assertion on posts model")
		}
		m.tokens = tokensModel
		cmd = newCmd

		if m.tokens.Exit {
			m.tokens = tokens.NewModel(m.cfg, m.dbpool, m.user)
			m.status = statusReady
		} else if m.tokens.Quit {
			m.status = statusQuitting
			return m, tea.Quit
		}
	case statusSettingUsername:
		m.username, cmd = username.Update(msg, m.username)
		if m.username.Done {
			m.username = username.NewModel(m.dbpool, m.user, m.sshUser) // reset the state
			m.status = statusReady
		} else if m.username.Quit {
			m.status = statusQuitting
			return m, tea.Quit
		}
	case statusNoAccount:
		m.createAccount, cmd = account.Update(msg, m.createAccount)
		if m.createAccount.Done {
			m.createAccount = account.NewCreateModel(m.cfg, m.dbpool, m.publicKey) // reset the state
			m.status = statusReady
		} else if m.createAccount.Quit {
			m.status = statusQuitting
			return m, tea.Quit
		}
	}

	// Handle the menu
	switch m.menuChoice {
	case setUserChoice:
		m.status = statusSettingUsername
		m.menuChoice = unsetChoice
		cmd = username.InitialCmd()
	case postsChoice:
		m.status = statusBrowsingPosts
		m.menuChoice = unsetChoice
		cmd = posts.LoadPosts(m.posts)
	case keysChoice:
		m.status = statusBrowsingKeys
		m.menuChoice = unsetChoice
		cmd = keys.LoadKeys(m.keys)
	case tokensChoice:
		m.status = statusBrowsingTokens
		m.menuChoice = unsetChoice
		cmd = tokens.LoadKeys(m.tokens)
	case exitChoice:
		m.status = statusQuitting
		m.dbpool.Close()
		cmd = tea.Quit
	}

	return m, cmd
}

func (m model) menuView() string {
	var s string
	for i := 0; i < len(menuChoices); i++ {
		e := "  "
		menuItem := menuChoices[menuChoice(i)]
		if i == m.menuIndex {
			e = m.styles.SelectionMarker.String() +
				m.styles.SelectedMenuItem.Render(menuItem)
		} else {
			e += menuItem
		}
		if i < len(menuChoices)-1 {
			e += "\n"
		}
		s += e
	}

	return s
}

func footerView(m model) string {
	if m.err != nil {
		return m.errorView(m.err)
	}
	return "\n\n" + common.HelpView("j/k, ↑/↓: choose", "enter: select")
}

func (m model) errorView(err error) string {
	head := m.styles.Error.Render("Error: ")
	body := m.styles.Subtle.Render(err.Error())
	msg := m.styles.Wrap.Render(head + body)
	return "\n\n" + indent.String(msg, 2)
}

func (m model) View() string {
	w := m.terminalSize.Width - m.styles.App.GetHorizontalFrameSize()
	s := m.styles.Logo.SetString(m.cfg.Domain).String() + "\n\n"
	switch m.status {
	case statusNoAccount:
		s += account.View(m.createAccount)
	case statusReady:
		s += m.info.View()
		s += "\n\n" + m.menuView()
		s += footerView(m)
	case statusSettingUsername:
		s += username.View(m.username)
	case statusBrowsingPosts:
		s += m.posts.View()
	case statusBrowsingKeys:
		s += m.keys.View()
	case statusBrowsingTokens:
		s += m.tokens.View()
	}
	return m.styles.App.Render(wrap.String(wordwrap.String(s, w), w))
}
