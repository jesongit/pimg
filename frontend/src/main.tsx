import React from "react";
import ReactDOM from "react-dom/client";
import { createBrowserRouter, RouterProvider, Navigate } from "react-router-dom";
import "@fontsource/ibm-plex-mono/400.css";
import "@fontsource/ibm-plex-mono/500.css";
import "./styles/tokens.css";
import "./styles/base.css";
import "./styles/components.css";
import App from "./App";
import Home from "./pages/Home";
import { PageLoading, RouteLoadError } from "./RouteLoading";

const Login = React.lazy(() => import("./pages/Login"));
const AdminApp = React.lazy(() => import("./admin/AdminApp"));
import "./styles/surfaces.css";
import "./admin/library-toolbar-layout.css";

const router = createBrowserRouter([
  {
    path: "/",
    element: <App />,
    errorElement: <RouteLoadError />,
    children: [
      { index: true, element: <Home /> },
      { path: "login", element: <React.Suspense fallback={<PageLoading />}><Login /></React.Suspense> },
      { path: "admin/*", element: <React.Suspense fallback={<PageLoading />}><AdminApp /></React.Suspense> },
      { path: "library/*", element: <React.Suspense fallback={<PageLoading />}><AdminApp /></React.Suspense> },
      { path: "*", element: <Navigate to="/" replace /> },
    ],
  },
]);

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <RouterProvider router={router} />
  </React.StrictMode>
);
