import { AwsClient } from "aws4fetch";

const TOKEN_VERSION = 1;
const encoder = new TextEncoder();
const decoder = new TextDecoder();

export default {
    async fetch(request, env) {
        const url = new URL(request.url);

        if (request.method === "OPTIONS") {
            return preflight(request, env);
        }
        if (request.method !== "GET" && request.method !== "HEAD") {
            return errorResponse(405, "Method not allowed", env, request);
        }
        if (!url.pathname.startsWith("/d/")) {
            return errorResponse(404, "Not found", env, request);
        }

        let claims;
        try {
            claims = await verifyToken(url.pathname.slice(3), env);
        } catch (err) {
            return errorResponse(403, "Invalid or expired download token", env, request);
        }

        if (claims.e < Math.floor(Date.now() / 1000)) {
            return errorResponse(403, "Download token expired", env, request);
        }
        if (typeof claims.k !== "string" || claims.k.length === 0 ||
            typeof claims.n !== "string" || claims.n.length === 0) {
            return errorResponse(403, "Invalid download token payload", env, request);
        }
        const keyPrefix = String(env.B2_KEY_PREFIX || "");
        if (!keyPrefix.endsWith("/") || !claims.k.startsWith(keyPrefix)) {
            return errorResponse(403, "Object is outside the configured prefix", env, request);
        }

        let b2URL;
        try {
            b2URL = objectURL(env, claims.k);
        } catch (err) {
            return errorResponse(500, "Storage is not configured", env, request);
        }
        const upstreamHeaders = new Headers();
        for (const name of ["range", "if-range", "if-none-match", "if-modified-since"]) {
            const value = request.headers.get(name);
            if (value) upstreamHeaders.set(name, value);
        }

        const aws = new AwsClient({
            accessKeyId: env.B2_ACCESS_KEY_ID,
            secretAccessKey: env.B2_SECRET_ACCESS_KEY,
            service: "s3",
            region: env.B2_REGION,
        });

        let upstream;
        try {
            const signed = await aws.sign(b2URL, {
                method: request.method,
                headers: upstreamHeaders,
            });
            upstream = await fetch(signed);
        } catch (err) {
            return errorResponse(502, "Could not reach storage", env, request);
        }

        if (upstream.status === 404) {
            return errorResponse(404, "Object not found", env, request);
        }
        if (upstream.status === 403) {
            return errorResponse(403, "Storage denied the request", env, request);
        }
        if (upstream.status >= 400 && upstream.status !== 416) {
            return errorResponse(502, "Storage returned an error", env, request);
        }

        const headers = new Headers();
        for (const name of [
            "content-type",
            "content-length",
            "content-range",
            "accept-ranges",
            "etag",
            "last-modified",
        ]) {
            const value = upstream.headers.get(name);
            if (value) headers.set(name, value);
        }
        if (!headers.has("content-type")) {
            headers.set("content-type", "application/octet-stream");
        }
        if (!headers.has("accept-ranges")) {
            headers.set("accept-ranges", "bytes");
        }
        headers.set("content-disposition", contentDisposition(claims.n));
        headers.set("cache-control", "private, no-store");
        headers.set("cdn-cache-control", "no-store");
        headers.set("referrer-policy", "no-referrer");
        headers.set("x-content-type-options", "nosniff");
        addCORS(headers, env, request);

        return new Response(request.method === "HEAD" ? null : upstream.body, {
            status: upstream.status,
            headers,
        });
    },
};

async function verifyToken(token, env) {
    const parts = token.split(".");
    if (parts.length !== 3 || parts[0] !== "v1") {
        throw new Error("bad token format");
    }

    const secret = env.DOWNLOAD_TOKEN_SECRET;
    if (!secret || secret.length < 32) {
        throw new Error("worker token secret is not configured");
    }

    const body = base64URLDecode(parts[1]);
    const signature = base64URLDecode(parts[2]);
    const key = await crypto.subtle.importKey(
        "raw",
        encoder.encode(secret),
        { name: "HMAC", hash: "SHA-256" },
        false,
        ["verify"],
    );
    const valid = await crypto.subtle.verify("HMAC", key, signature, body);
    if (!valid) {
        throw new Error("bad signature");
    }

    const claims = JSON.parse(decoder.decode(body));
    if (claims.v !== TOKEN_VERSION || typeof claims.e !== "number") {
        throw new Error("bad payload");
    }
    return claims;
}

function objectURL(env, key) {
    const endpoint = String(env.B2_ENDPOINT || "").replace(/\/+$/, "");
    if (!endpoint.startsWith("https://") || !env.B2_BUCKET) {
        throw new Error("B2 endpoint or bucket is not configured");
    }
    const bucket = encodeURIComponent(env.B2_BUCKET);
    const encodedKey = key.split("/").map(encodeURIComponent).join("/");
    return `${endpoint}/${bucket}/${encodedKey}`;
}

function base64URLDecode(value) {
    const normalized = value.replace(/-/g, "+").replace(/_/g, "/");
    const padding = normalized.length % 4;
    const padded = normalized + (padding ? "=".repeat(4 - padding) : "");
    const binary = atob(padded);
    const bytes = new Uint8Array(binary.length);
    for (let i = 0; i < binary.length; i += 1) {
        bytes[i] = binary.charCodeAt(i);
    }
    return bytes;
}

function contentDisposition(filename) {
    const name = String(filename || "file");
    const fallback = name
        .replace(/[^\x20-\x7e]/g, "_")
        .replace(/["\\\r\n]/g, "_");
    const encoded = encodeURIComponent(name).replace(/['()*]/g, (char) =>
        "%" + char.charCodeAt(0).toString(16).toUpperCase(),
    );
    return `attachment; filename="${fallback}"; filename*=UTF-8''${encoded}`;
}

function addCORS(headers, env, request) {
    if (!env.CORS_ORIGIN) return;
    const origin = request.headers.get("Origin");
    if (origin && origin !== env.CORS_ORIGIN) return;
    headers.set("access-control-allow-origin", env.CORS_ORIGIN);
    headers.set("access-control-expose-headers", "Content-Length, Content-Range, Content-Disposition, ETag, Accept-Ranges");
    headers.set("vary", "Origin");
}

function preflight(request, env) {
    const origin = request.headers.get("Origin");
    if (!env.CORS_ORIGIN || origin !== env.CORS_ORIGIN) {
        return new Response("CORS is not configured for this origin", { status: 403 });
    }
    return new Response(null, {
        status: 204,
        headers: {
            "access-control-allow-origin": env.CORS_ORIGIN,
            "access-control-allow-methods": "GET, HEAD, OPTIONS",
            "access-control-allow-headers": "Range, If-Range",
            "access-control-max-age": "86400",
            "vary": "Origin",
        },
    });
}

function errorResponse(status, message, env, request) {
    const headers = new Headers({
        "content-type": "text/plain; charset=utf-8",
        "cache-control": "no-store",
        "x-content-type-options": "nosniff",
    });
    addCORS(headers, env, request);
    return new Response(message + "\n", { status, headers });
}
