using System;
using System.Diagnostics;
using System.IO;

// Build as a Windows application, not a console application. The scheduled task
// must enter here before PowerShell has a chance to allocate a terminal.
internal static class WindowsServiceLauncher
{
    private static int Main(string[] args)
    {
        try
        {
            if (args.Length != 2)
            {
                throw new ArgumentException("Expected PowerShell executable and service script paths.");
            }

            string script = Path.GetFullPath(args[1]);
            var start = new ProcessStartInfo
            {
                FileName = Path.GetFullPath(args[0]),
                Arguments = "-NoLogo -NoProfile -NonInteractive -WindowStyle Hidden -File \"" + script + "\"",
                WorkingDirectory = Path.GetDirectoryName(script),
                UseShellExecute = false,
                CreateNoWindow = true
            };

            using (Process child = Process.Start(start))
            {
                child.WaitForExit();
                return child.ExitCode;
            }
        }
        catch (Exception error)
        {
            string directory = Path.GetFullPath(Path.Combine(AppDomain.CurrentDomain.BaseDirectory, "..", "logs"));
            Directory.CreateDirectory(directory);
            File.AppendAllText(Path.Combine(directory, "launcher-host.log"),
                DateTimeOffset.Now.ToString("o") + " " + error + Environment.NewLine);
            return 1;
        }
    }
}
