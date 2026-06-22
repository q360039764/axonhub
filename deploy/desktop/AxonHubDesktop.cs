using System;
using System.Diagnostics;
using System.Drawing;
using System.IO;
using System.Net;
using System.Threading.Tasks;
using System.Windows.Forms;
using Microsoft.Web.WebView2.Core;
using Microsoft.Web.WebView2.WinForms;

internal static class Program
{
    // 桌面窗口默认访问本机 AxonHub 服务地址。
    private const string DefaultServerUrl = "http://localhost:8090";

    [STAThread]
    private static void Main(string[] args)
    {
        Application.EnableVisualStyles();
        Application.SetCompatibleTextRenderingDefault(false);
        Application.Run(new MainForm(DesktopOptions.FromArgs(args, DefaultServerUrl)));
    }
}

internal sealed class DesktopOptions
{
    public string ServerUrl { get; private set; }
    public bool NoStart { get; private set; }
    public bool ExitOnClose { get; private set; }

    public static DesktopOptions FromArgs(string[] args, string defaultServerUrl)
    {
        DesktopOptions options = new DesktopOptions();
        string envServerUrl = Environment.GetEnvironmentVariable("AXONHUB_DESKTOP_URL");
        options.ServerUrl = NormalizeServerUrl(string.IsNullOrWhiteSpace(envServerUrl) ? defaultServerUrl : envServerUrl);

        for (int i = 0; i < args.Length; i++)
        {
            string arg = args[i];
            if (string.Equals(arg, "--no-start", StringComparison.OrdinalIgnoreCase))
            {
                options.NoStart = true;
                continue;
            }

            if (string.Equals(arg, "--exit-on-close", StringComparison.OrdinalIgnoreCase))
            {
                options.ExitOnClose = true;
                continue;
            }

            if (arg.StartsWith("--url=", StringComparison.OrdinalIgnoreCase))
            {
                options.ServerUrl = NormalizeServerUrl(arg.Substring("--url=".Length));
                continue;
            }

            if (string.Equals(arg, "--url", StringComparison.OrdinalIgnoreCase) && i + 1 < args.Length)
            {
                i++;
                options.ServerUrl = NormalizeServerUrl(args[i]);
            }
        }

        return options;
    }

    private static string NormalizeServerUrl(string value)
    {
        string normalized = (value ?? string.Empty).Trim();
        if (normalized.EndsWith("/", StringComparison.Ordinal))
        {
            normalized = normalized.TrimEnd('/');
        }

        return string.IsNullOrEmpty(normalized) ? "http://localhost:8090" : normalized;
    }
}

internal sealed class MainForm : Form
{
    private const string AppTitle = "AxonHub Desktop";
    private const int ServerWaitSeconds = 45;
    private const int ServerProbeDelayMs = 800;

    private readonly DesktopOptions options;
    private readonly string baseDir;
    private readonly string backendPath;
    private readonly string pidFilePath;

    private WebView2 webView;
    private Label statusLabel;
    private NotifyIcon trayIcon;
    private Process backendProcess;
    private bool backendStartedByDesktop;
    private bool allowExit;

    public MainForm(DesktopOptions options)
    {
        this.options = options;
        baseDir = AppDomain.CurrentDomain.BaseDirectory;
        backendPath = Path.Combine(baseDir, "axonhub.exe");
        pidFilePath = Path.Combine(baseDir, "axonhub.pid");

        Text = AppTitle;
        StartPosition = FormStartPosition.CenterScreen;
        MinimumSize = new Size(1100, 720);
        Size = new Size(1440, 900);
        Icon = SystemIcons.Application;

        BuildUi();
        BuildTrayIcon();

        Load += async delegate { await StartDesktopAsync(); };
        FormClosing += OnFormClosing;
    }

    protected override void Dispose(bool disposing)
    {
        if (disposing)
        {
            if (trayIcon != null)
            {
                trayIcon.Visible = false;
                trayIcon.Dispose();
                trayIcon = null;
            }

            if (webView != null)
            {
                webView.Dispose();
                webView = null;
            }
        }

        base.Dispose(disposing);
    }

    private void BuildUi()
    {
        webView = new WebView2();
        webView.Dock = DockStyle.Fill;
        webView.Visible = false;
        Controls.Add(webView);

        statusLabel = new Label();
        statusLabel.Dock = DockStyle.Fill;
        statusLabel.TextAlign = ContentAlignment.MiddleCenter;
        statusLabel.Font = new Font("Microsoft YaHei UI", 12F, FontStyle.Regular, GraphicsUnit.Point);
        statusLabel.ForeColor = Color.FromArgb(52, 52, 52);
        statusLabel.Text = "正在启动 AxonHub...";
        Controls.Add(statusLabel);
        statusLabel.BringToFront();
    }

    private void BuildTrayIcon()
    {
        ContextMenuStrip menu = new ContextMenuStrip();
        menu.Items.Add("打开 AxonHub", null, delegate { ShowMainWindow(); });
        menu.Items.Add("刷新页面", null, delegate { ReloadWebView(); });
        menu.Items.Add("清理登录状态并刷新", null, ClearDesktopSession);
        menu.Items.Add(new ToolStripSeparator());
        menu.Items.Add("退出桌面窗口", null, delegate { ExitDesktop(false); });
        menu.Items.Add("停止服务并退出", null, delegate { ExitDesktop(true); });

        trayIcon = new NotifyIcon();
        trayIcon.Text = AppTitle;
        trayIcon.Icon = SystemIcons.Application;
        trayIcon.ContextMenuStrip = menu;
        trayIcon.Visible = true;
        trayIcon.DoubleClick += delegate { ShowMainWindow(); };
    }

    private async Task StartDesktopAsync()
    {
        try
        {
            SetStatus("正在检查 AxonHub 服务...");

            if (!options.NoStart)
            {
                StartBackendIfNeeded();
            }

            SetStatus("正在等待 AxonHub 页面就绪...");
            bool ready = await WaitForServerReadyAsync();
            if (!ready)
            {
                SetStatus("AxonHub 服务暂未就绪，请检查 axonhub.exe、config.yml 和 logs 目录。");
                MessageBox.Show(
                    "无法访问 " + options.ServerUrl + "。\r\n请确认 axonhub.exe 能正常启动，并查看 logs 目录中的日志。",
                    AppTitle,
                    MessageBoxButtons.OK,
                    MessageBoxIcon.Warning);
                return;
            }

            await InitializeWebViewAsync();
        }
        catch (Exception ex)
        {
            SetStatus("桌面窗口启动失败：" + ex.Message);
            MessageBox.Show(ex.Message, AppTitle, MessageBoxButtons.OK, MessageBoxIcon.Error);
        }
    }

    private void StartBackendIfNeeded()
    {
        Process running = FindRunningBackendProcess();
        if (running != null)
        {
            backendProcess = running;
            return;
        }

        if (!File.Exists(backendPath))
        {
            throw new FileNotFoundException("当前目录缺少 axonhub.exe，桌面窗口需要与 axonhub.exe 放在同一目录。", backendPath);
        }

        ProcessStartInfo startInfo = new ProcessStartInfo();
        startInfo.FileName = backendPath;
        startInfo.WorkingDirectory = baseDir;
        startInfo.UseShellExecute = true;
        startInfo.WindowStyle = ProcessWindowStyle.Hidden;

        backendProcess = Process.Start(startInfo);
        backendStartedByDesktop = backendProcess != null;

        if (backendProcess != null)
        {
            File.WriteAllText(pidFilePath, backendProcess.Id.ToString());
        }
    }

    private Process FindRunningBackendProcess()
    {
        Process processFromPid = FindProcessFromPidFile();
        if (processFromPid != null)
        {
            return processFromPid;
        }

        Process[] processes = Process.GetProcessesByName("axonhub");
        foreach (Process process in processes)
        {
            if (IsExpectedBackendProcess(process))
            {
                return process;
            }
        }

        return null;
    }

    private Process FindProcessFromPidFile()
    {
        try
        {
            if (!File.Exists(pidFilePath))
            {
                return null;
            }

            int pid;
            if (!int.TryParse(File.ReadAllText(pidFilePath).Trim(), out pid))
            {
                return null;
            }

            Process process = Process.GetProcessById(pid);
            return IsExpectedBackendProcess(process) ? process : null;
        }
        catch
        {
            return null;
        }
    }

    private bool IsExpectedBackendProcess(Process process)
    {
        try
        {
            string expected = Path.GetFullPath(backendPath);
            string actual = Path.GetFullPath(process.MainModule.FileName);
            return string.Equals(expected, actual, StringComparison.OrdinalIgnoreCase);
        }
        catch
        {
            return false;
        }
    }

    private async Task<bool> WaitForServerReadyAsync()
    {
        DateTime deadline = DateTime.UtcNow.AddSeconds(ServerWaitSeconds);
        while (DateTime.UtcNow < deadline)
        {
            if (await IsServerHealthyAsync())
            {
                return true;
            }

            await Task.Delay(ServerProbeDelayMs);
        }

        return false;
    }

    private Task<bool> IsServerHealthyAsync()
    {
        return Task.Factory.StartNew(delegate
        {
            try
            {
                HttpWebRequest request = (HttpWebRequest)WebRequest.Create(options.ServerUrl + "/health");
                request.Method = "GET";
                request.Timeout = 1500;
                request.ReadWriteTimeout = 1500;
                using (HttpWebResponse response = (HttpWebResponse)request.GetResponse())
                {
                    return (int)response.StatusCode >= 200 && (int)response.StatusCode < 500;
                }
            }
            catch
            {
                return false;
            }
        });
    }

    private async Task InitializeWebViewAsync()
    {
        try
        {
            string userDataFolder = Path.Combine(
                Environment.GetFolderPath(Environment.SpecialFolder.LocalApplicationData),
                "AxonHub",
                "DesktopWebView");
            Directory.CreateDirectory(userDataFolder);

            CoreWebView2Environment environment = await CoreWebView2Environment.CreateAsync(null, userDataFolder);
            await webView.EnsureCoreWebView2Async(environment);

            // 禁止普通用户误开调试面板，保留桌面窗口的应用形态。
            webView.CoreWebView2.Settings.AreDevToolsEnabled = false;
            webView.CoreWebView2.Settings.AreDefaultContextMenusEnabled = true;
            webView.CoreWebView2.Settings.IsStatusBarEnabled = false;
            // 桌面端使用独立 WebView2 数据目录，关闭自动填充避免旧密码被提交。
            webView.CoreWebView2.Settings.IsGeneralAutofillEnabled = false;
            webView.CoreWebView2.Settings.IsPasswordAutosaveEnabled = false;
            webView.CoreWebView2.NewWindowRequested += OnNewWindowRequested;
            webView.NavigationStarting += delegate { SetStatus("正在加载 AxonHub 页面..."); };
            webView.NavigationCompleted += OnNavigationCompleted;

            statusLabel.Visible = false;
            webView.Visible = true;
            webView.CoreWebView2.Navigate(options.ServerUrl);
        }
        catch (Exception ex)
        {
            SetStatus("WebView2 初始化失败：" + ex.Message);
            MessageBox.Show(
                "WebView2 初始化失败。\r\n请确认系统已安装 Microsoft Edge WebView2 Runtime。\r\n\r\n" + ex.Message,
                AppTitle,
                MessageBoxButtons.OK,
                MessageBoxIcon.Error);
        }
    }

    private void OnNewWindowRequested(object sender, CoreWebView2NewWindowRequestedEventArgs e)
    {
        e.Handled = true;
        if (e.Uri.StartsWith("http://", StringComparison.OrdinalIgnoreCase) ||
            e.Uri.StartsWith("https://", StringComparison.OrdinalIgnoreCase))
        {
            webView.CoreWebView2.Navigate(e.Uri);
            return;
        }

        Process.Start(e.Uri);
    }

    private void OnNavigationCompleted(object sender, CoreWebView2NavigationCompletedEventArgs e)
    {
        if (e.IsSuccess)
        {
            statusLabel.Visible = false;
            webView.Visible = true;
            return;
        }

        webView.Visible = false;
        SetStatus("页面加载失败，托盘菜单中可刷新页面。");
    }

    private void ReloadWebView()
    {
        if (webView != null && webView.CoreWebView2 != null)
        {
            webView.CoreWebView2.Reload();
        }
    }

    private async void ClearDesktopSession(object sender, EventArgs e)
    {
        if (webView == null || webView.CoreWebView2 == null)
        {
            return;
        }

        try
        {
            SetStatus("正在清理桌面登录状态...");
            // 清理桌面 WebView2 独立数据，避免旧 token、Cookie 或自动填充影响登录。
            await webView.CoreWebView2.Profile.ClearBrowsingDataAsync(CoreWebView2BrowsingDataKinds.AllProfile);
            webView.CoreWebView2.Navigate(options.ServerUrl);
        }
        catch (Exception ex)
        {
            MessageBox.Show("清理桌面登录状态失败：\r\n" + ex.Message, AppTitle, MessageBoxButtons.OK, MessageBoxIcon.Warning);
        }
    }

    private void ShowMainWindow()
    {
        Show();
        WindowState = FormWindowState.Normal;
        Activate();
    }

    private void OnFormClosing(object sender, FormClosingEventArgs e)
    {
        if (allowExit || options.ExitOnClose)
        {
            return;
        }

        e.Cancel = true;
        Hide();
        trayIcon.ShowBalloonTip(2000, AppTitle, "AxonHub 已最小化到系统托盘。", ToolTipIcon.Info);
    }

    private void ExitDesktop(bool stopBackend)
    {
        allowExit = true;
        if (stopBackend)
        {
            StopBackend();
        }

        Close();
    }

    private void StopBackend()
    {
        Process process = backendProcess ?? FindRunningBackendProcess();
        if (process == null)
        {
            return;
        }

        try
        {
            if (!process.HasExited && (backendStartedByDesktop || IsExpectedBackendProcess(process)))
            {
                process.Kill();
                process.WaitForExit(5000);
            }
        }
        catch
        {
        }

        try
        {
            if (File.Exists(pidFilePath))
            {
                File.Delete(pidFilePath);
            }
        }
        catch
        {
        }
    }

    private void SetStatus(string text)
    {
        statusLabel.Text = text;
        statusLabel.Visible = true;
        statusLabel.BringToFront();
    }
}
