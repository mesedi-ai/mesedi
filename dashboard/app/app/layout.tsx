import AuthGate from "./_components/AuthGate";
import Sidebar from "./_components/Sidebar";
import Topbar from "./_components/Topbar";

export default function AppLayout({
  children,
}: Readonly<{
  children: React.ReactNode;
}>) {
  return (
    <AuthGate>
      <div
        className="flex min-h-screen"
        style={{ background: "var(--bg)", color: "var(--text)" }}
      >
        <Sidebar />
        <div className="flex-1 flex flex-col min-w-0">
          <Topbar />
          <main className="flex-1 overflow-auto">{children}</main>
        </div>
      </div>
    </AuthGate>
  );
}
