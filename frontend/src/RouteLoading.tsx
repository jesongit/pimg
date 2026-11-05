import { Link } from "react-router-dom";

export function PageLoading() {
  return <div className="auth-pending" role="status">正在加载页面…</div>;
}

// A stale deployment tab or a failed chunk request must offer a recovery path.
export function RouteLoadError() {
  return <main className="main">
    <div role="alert">
      <p>页面加载失败，请重新加载后重试。</p>
      <button type="button" className="btn" onClick={() => window.location.reload()}>重新加载</button>{" "}
      <Link to="/" className="btn btn-ghost">返回首页</Link>
    </div>
  </main>;
}
