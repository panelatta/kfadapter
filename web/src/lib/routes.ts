export type Route = "/" | "/signin";

export function routeFromLocation(): Route {
    return window.location.pathname === "/signin" ? "/signin" : "/";
}

export function replaceRoute(route: Route): void {
    if (window.location.pathname !== route)
        window.history.replaceState(null, "", route);
    window.dispatchEvent(new PopStateEvent("popstate"));
}
