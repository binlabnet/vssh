package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/stephane-martin/vssh/crypto"
	"github.com/stephane-martin/vssh/params"
	"github.com/stephane-martin/vssh/remoteops"
	"github.com/stephane-martin/vssh/sys"
	"github.com/stephane-martin/vssh/widgets"

	"github.com/gdamore/tcell"
	"github.com/rivo/tview"
	gssh "github.com/stephane-martin/golang-ssh"
	"github.com/urfave/cli"
	"golang.org/x/sync/errgroup"
)

func TopCommand() cli.Command {
	return cli.Command{
		Name:   "top",
		Action: topAction,
	}
}

func flex() *tview.Flex {
	f := tview.NewFlex()
	f.SetBackgroundColor(tview.Styles.PrimitiveBackgroundColor)
	return f
}

func textView() *tview.TextView {
	t := tview.NewTextView()
	t.SetBackgroundColor(tview.Styles.PrimitiveBackgroundColor)
	t.SetScrollable(false)
	t.SetBorder(false)
	t.SetDynamicColors(true)
	return t
}

func fmtUptime(up time.Duration) string {
	days := int(up.Hours() / 24)
	hours := int(up.Hours() - float64(days*24))
	minutes := int(up.Minutes() - float64((days*24*60)+(hours*60)))
	seconds := int(up.Seconds() - float64((days*24*60*60)+(hours*60*60)+(minutes*60)))
	return fmt.Sprintf("%d days, %d hours, %d minutes, %d seconds", days, hours, minutes, seconds)
}

func topAction(clictx *cli.Context) (e error) {
	defer func() {
		if e != nil {
			e = cli.NewExitError(e.Error(), 1)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sys.CancelOnSignal(cancel)

	gparams := params.Params{
		LogLevel: strings.ToLower(strings.TrimSpace(clictx.GlobalString("loglevel"))),
	}

	logger, err := params.Logger(gparams.LogLevel)
	if err != nil {
		return err
	}
	defer func() { _ = logger.Sync() }()

	c := params.NewCliContext(clictx)
	if c.SSHHost() == "" {
		var err error
		c, err = widgets.Form(c, true)
		if err != nil {
			return err
		}
	}

	sshParams, err := params.GetSSHParams(c)
	if err != nil {
		return err
	}

	_, credentials, err := crypto.GetSSHCredentials(ctx, c, sshParams.LoginName, sshParams.UseAgent, logger)
	if err != nil {
		return err
	}
	methods := crypto.CredentialsToMethods(credentials, logger)
	if len(methods) == 0 {
		return errors.New("no usable credentials")
	}

	cfg := gssh.Config{
		User:      sshParams.LoginName,
		Host:      sshParams.Host,
		Port:      sshParams.Port,
		Auth:      methods,
		HTTPProxy: sshParams.HTTPProxy,
	}
	hkcb, err := gssh.MakeHostKeyCallback(sshParams.Insecure, logger)
	if err != nil {
		return err
	}
	cfg.HostKey = hkcb
	client, err := gssh.Dial(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	stater, err := remoteops.NewStater(client)
	if err != nil {
		return err
	}
	stats := make(chan remoteops.Stats)
	g, lctx := errgroup.WithContext(ctx)

	app := tview.NewApplication()
	v := tview.NewFlex()
	v.SetDirection(tview.FlexRow)
	v.SetBorder(true)
	v.SetTitleColor(tview.Styles.TitleColor)
	v.SetBackgroundColor(tview.Styles.PrimitiveBackgroundColor)
	v.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape || event.Rune() == 'q' {
			app.Stop()
			return nil
		}
		return event
	})

	h1 := flex()
	h1.SetBorderPadding(1, 0, 0, 0)
	header := textView().SetTextAlign(tview.AlignCenter)
	h1.AddItem(header, 0, 1, false)

	h4 := flex()
	h4.SetBorderPadding(1, 0, 0, 0)

	filesystems := textView()
	filesystems.SetBorder(true)
	filesystems.SetBorderPadding(1, 1, 1, 1)
	filesystems.SetTitle(" Filesystems (unit: MB)")
	filesystems.SetTitleColor(tview.Styles.ContrastSecondaryTextColor)
	h4.AddItem(filesystems, 0, 1, false)

	interfaces := textView()
	interfaces.SetBorder(true)
	interfaces.SetBorderPadding(1, 1, 1, 1)
	interfaces.SetTitleColor(tview.Styles.ContrastSecondaryTextColor)
	interfaces.SetTitle(" Interfaces ")
	h4.AddItem(interfaces, 0, 1, false)

	v.AddItem(h1, 6, 0, false)
	v.AddItem(h4, 0, 10, false)

	g.Go(func() error {
		defer func() {
			close(stats)
		}()
		for {
			s, err := stater.Get(lctx)
			if err != nil {
				return err
			}
			select {
			case <-lctx.Done():
				return context.Canceled
			case stats <- s:
			}
			select {
			case <-lctx.Done():
				return context.Canceled
			case <-time.After(5 * time.Second):
			}
		}
	})

	g.Go(func() error {
		for {
			select {
			case <-lctx.Done():
				return context.Canceled
			case s, ok := <-stats:
				if !ok {
					return nil
				}
				app.QueueUpdateDraw(func() {
					v.SetTitle(fmt.Sprintf(" %s ", s.Hostname))
					var buf strings.Builder
					buf.WriteString(fmt.Sprintf("[lightcoral]Uptime[-]: [yellowgreen]%s[-]\n", fmtUptime(s.Uptime)))
					buf.WriteString(
						fmt.Sprintf(
							"[lightcoral]Load[-]: [yellowgreen]%s[-][1m] [yellowgreen]%s[-][5m] [yellowgreen]%s[-][10m]\n",
							s.Load.Load1, s.Load.Load5, s.Load.Load10,
						),
					)
					buf.WriteString(
						fmt.Sprintf(
							"[lightcoral]RAM[-]: active = [darkorange]%d[-] MB / [navajowhite]%d[-] MB\n",
							s.Mem.MemActive/(1024*1024), s.Mem.MemTotal/(1024*1024),
						),
					)
					buf.WriteString(
						fmt.Sprintf(
							"[lightcoral]Swap[-]: active = [darkorange]%d[-] MB / [navajowhite]%d[-] MB\n",
							(s.Mem.SwapTotal-s.Mem.SwapFree)/(1024*1024), s.Mem.MemTotal/(1024*1024),
						),
					)
					buf.WriteString(
						fmt.Sprintf(
							"[lightcoral]Processes[-]: running = [yellowgreen]%s[-] / [navajowhite]%s[-]",
							s.Load.RunningProcs, s.Load.TotalProcs,
						),
					)
					header.SetText(buf.String())
					var mpLen int
					var maxUsed uint64
					var maxTotal uint64
					for _, fs := range s.FS {
						if len(fs.MountPoint) > mpLen {
							mpLen = len(fs.MountPoint)
						}
						if fs.Used > maxUsed {
							maxUsed = fs.Used
						}
						if fs.Total() > maxTotal {
							maxTotal = fs.Total()
						}
					}
					usedLen := len(fmt.Sprintf("%d", maxUsed/(1024*1024)))
					totalLen := len(fmt.Sprintf("%d", maxTotal/(1024*1024)))
					usedFmt := fmt.Sprintf("%%-%dd", usedLen)
					totalFmt := fmt.Sprintf("%%-%dd", totalLen)
					mpFmt := fmt.Sprintf("%%-%ds", mpLen)
					buf.Reset()
					for _, fs := range s.FS {
						percent := 100 * float64(fs.Used) / float64(fs.Total())
						percentStr := fmt.Sprintf("%.1f%%", percent)
						if percent >= 90 {
							percentStr = fmt.Sprintf("[orange]%.1f%%[-]", percent)
						} else if percent >= 95 {
							percentStr = fmt.Sprintf("[red]%.1f%%[-]", percent)
						}
						buf.WriteString(
							fmt.Sprintf(
								"[lightblue]"+mpFmt+"[-] [orange]"+usedFmt+"[-] / [navajowhite]"+totalFmt+"[-] (%s)\n",
								fs.MountPoint,
								fs.Used/(1024*1024),
								fs.Total()/(1024*1024),
								percentStr,
							),
						)
					}
					filesystems.SetText(buf.String())

					buf.Reset()
					for _, iface := range s.Net {
						var addresses []string
						addresses = append(addresses, iface.IPv4...)
						addresses = append(addresses, iface.IPv6...)
						for i := range addresses {
							addresses[i] = fmt.Sprintf("[navajowhite]%s[-]", addresses[i])
						}
						buf.WriteString(fmt.Sprintf("[lightblue]%s[-]\n", iface.Name))
						buf.WriteString("├─ IP: ")
						buf.WriteString(strings.Join(addresses, ", "))
						buf.WriteString("\n└─ ")
						buf.WriteString(fmt.Sprintf("Rx: %.2f / Tx: %.2f\n", float64(iface.Rx)/(1024*1024), float64(iface.Tx)/(1024*1024)))
					}
					interfaces.SetText(buf.String())

				})
			}

		}
	})

	g.Go(func() error {
		err := app.SetRoot(v, true).Run()
		if err == nil {
			return context.Canceled
		}
		return err
	})

	g.Go(func() error {
		<-lctx.Done()
		app.Stop()
		return nil
	})

	err = g.Wait()
	if err == context.Canceled {
		return nil
	}
	return err

}
