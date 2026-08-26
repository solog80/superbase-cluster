# /etc/varnish/default.vcl — Salt TV Supabase origin shield (us1 primary)
#
# Origin shield in front of Envoy (:8555). Caches at the origin so that
# Cloudflare-edge MISSes hit a fast local cache instead of PostgREST/Postgres.
# Envoy does prefix_rewrite (e.g. /storage/v1/object/public/x -> /x), so the
# backend URL no longer matches the client URL. We set an X-Cache-Rule marker
# in vcl_recv (on req.http) which is inherited by bereq.http, then act on it
# in vcl_backend_response.
#
#   /storage/v1/object/public/*  → public images, 4h TTL
#   GET /rest/v1/* (anon key)    → PostgREST reads, 60s TTL
# Everything else passes through (auth, writes, /api/v1 logic, /functions).

vcl 4.1;

backend envoy {
    .host = "127.0.0.1";
    .port = "8555";
}

acl purge {
    "127.0.0.1";
    "localhost";
    "10.0.0.0"/8;
    "172.16.0.0"/12;
    "100.64.0.0"/10;
}

# The Supabase anon key (public client key — safe to embed). If it's ever
# rotated, update this value and reload Varnish. Requests presenting this key
# are "anon" and their GET reads can be cached.
sub vcl_recv {
    if (req.method == "PURGE") {
        if (client.ip ~ purge) {
            return (purge);
        }
        return (synth(405, "Not allowed"));
    }

    # WebSocket upgrades (Supabase Realtime) — pipe through, never cache.
    if (req.http.Upgrade ~ "(?i)websocket") {
        return (pipe);
    }

    if (req.method != "GET" && req.method != "HEAD") {
        return (pass);
    }

    # Public storage images — cache aggressively (no auth needed).
    if (req.url ~ "^/storage/v1/object/public/") {
        unset req.http.cookie;
        unset req.http.Authorization;
        set req.http.X-Cache-Rule = "storage";
        return (hash);
    }

    # Anon PostgREST reads — cache when the caller presents the anon key
    # (either via apikey header or Authorization: Bearer). Any other key
    # (service-role, authenticated) passes through uncached.
    if (req.url ~ "^/rest/v1/") {
        set req.http.X-Is-Anon = "false";
        if (req.http.Authorization ~ "^Bearer ") {
            set req.http.X-Is-Anon = "false";
        }
        if (req.http.apikey == "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJyb2xlIjoiYW5vbiIsImlzcyI6InN1cGFiYXNlIiwiaWF0IjoxNzg3MzM2Mjk0LCJleHAiOjE5NDUwMTYyOTR9.9YCCl_oRCYHQIR3eAhUeLF-SiBqGIxaT9WqCS-YFtNw") {
            set req.http.X-Is-Anon = "true";
        }
        if (req.http.Authorization == "Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJyb2xlIjoiYW5vbiIsImlzcyI6InN1cGFiYXNlIiwiaWF0IjoxNzg3MzM2Mjk0LCJleHAiOjE5NDUwMTYyOTR9.9YCCl_oRCYHQIR3eAhUeLF-SiBqGIxaT9WqCS-YFtNw") {
            set req.http.X-Is-Anon = "true";
        }
        if (req.http.X-Is-Anon == "true") {
            unset req.http.cookie;
            set req.http.X-Cache-Rule = "rest";
            unset req.http.X-Is-Anon;
            return (hash);
        }
        unset req.http.X-Is-Anon;
        return (pass);
    }

    return (pass);
}

sub vcl_backend_response {
    if (bereq.http.X-Cache-Rule == "storage") {
        set beresp.ttl = 4h;
        set beresp.grace = 1h;
        unset beresp.http.Set-Cookie;
        unset beresp.http.Cookie;
        unset beresp.http.Cache-Control;
        set beresp.uncacheable = false;
    }

    if (bereq.http.X-Cache-Rule == "rest" && beresp.status == 200) {
        set beresp.ttl = 60s;
        set beresp.grace = 30s;
        unset beresp.http.Set-Cookie;
        unset beresp.http.Cookie;
        set beresp.uncacheable = false;
    }

    if (beresp.status >= 400) {
        set beresp.ttl = 0s;
    }
}

# Websocket upgrade (Supabase Realtime): Varnish strips the hop-by-hop
# Upgrade/Connection headers when piping, so re-add them for the backend.
sub vcl_pipe {
    if (req.http.Upgrade ~ "(?i)websocket") {
        set bereq.http.Connection = "Upgrade";
        set bereq.http.Upgrade = "websocket";
    }
}

sub vcl_deliver {
    if (obj.hits > 0) {
        set resp.http.X-Cache = "HIT";
    } else {
        set resp.http.X-Cache = "MISS";
    }
    set resp.http.X-Cache-Hits = obj.hits;
}
