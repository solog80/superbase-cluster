vcl 4.1;

backend replica {
    .host = "127.0.0.1";
    .port = "5555";
}

sub vcl_recv {
    if (req.method == "PURGE") {
        return (synth(405, "Not allowed"));
    }
    if (req.method != "GET" && req.method != "HEAD") {
        return (pass);
    }
    if (req.url ~ "^/rest/v1/(.*)") {
        set req.url = "/" + regsub(req.url, "^/rest/v1/", "");
        return (hash);
    }
    return (pass);
}

sub vcl_backend_response {
    if (beresp.status >= 500) {
        set beresp.ttl = 0s;
        return (deliver);
    }
    if (beresp.http.Set-Cookie) {
        return (pass);
    }
    set beresp.ttl = 60s;
    set beresp.grace = 30s;
    return (deliver);
}

sub vcl_deliver {
    if (obj.hits > 0) {
        set resp.http.X-Cache = "HIT";
    } else {
        set resp.http.X-Cache = "MISS";
    }
}
