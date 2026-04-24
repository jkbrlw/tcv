#!/usr/bin/env node
const http = require('http');
const https = require('https');
const url = require('url');
const fs = require('fs');
const net = require('net');

// Load policy from mounted files
const POLICY_DIR = process.env.POLICY_DIR || '/policy';
const BASELINE_POLICY = process.env.BASELINE_POLICY || '/app/baseline-policy.json';
const LOG_FILE = process.env.LOG_FILE || '/logs/proxy.log';
const ALLOW_LOCAL = process.env.ALLOW_LOCAL !== 'false'; // Default: allow local traffic

let allowedHosts = new Set();
let allowedIPs = new Set();
let localDomains = new Set();
let localPorts = new Set();

// Check if an IP is in a private/local range
function isPrivateIP(ip) {
  if (!net.isIP(ip)) return false;

  // IPv4 private ranges
  const parts = ip.split('.').map(Number);
  if (parts.length === 4) {
    // 10.0.0.0/8
    if (parts[0] === 10) return true;
    // 172.16.0.0/12
    if (parts[0] === 172 && parts[1] >= 16 && parts[1] <= 31) return true;
    // 192.168.0.0/16
    if (parts[0] === 192 && parts[1] === 168) return true;
    // 127.0.0.0/8 (loopback)
    if (parts[0] === 127) return true;
    // 169.254.0.0/16 (link-local)
    if (parts[0] === 169 && parts[1] === 254) return true;
  }

  // IPv6 loopback and link-local
  if (ip === '::1') return true;
  if (ip.startsWith('fe80:')) return true;
  if (ip.startsWith('fc') || ip.startsWith('fd')) return true; // ULA

  return false;
}

function loadPolicies() {
  const newHosts = new Set();
  const newIPs = new Set();
  const newLocalDomains = new Set();
  const newLocalPorts = new Set();

  // Load baseline policy first (always present, baked into image)
  try {
    if (fs.existsSync(BASELINE_POLICY)) {
      const baseline = JSON.parse(fs.readFileSync(BASELINE_POLICY, 'utf8'));
      const hosts = baseline.network?.allow_hosts || [];
      const ips = baseline.network?.allow_ips || [];
      const domains = baseline.local_domains || [];
      const ports = baseline.local_ports || [];
      hosts.forEach(h => newHosts.add(h));
      ips.forEach(ip => newIPs.add(ip));
      domains.forEach(d => newLocalDomains.add(d.toLowerCase()));
      ports.forEach(p => newLocalPorts.add(Number(p)));
      log(`Loaded baseline policy: ${hosts.length} hosts, ${ips.length} IPs, ${domains.length} local domains, ${ports.length} local ports`);
    }
  } catch (err) {
    log(`WARNING: Failed to load baseline policy: ${err.message}`);
  }

  // Read all .json files from policy directory (project-specific)
  let files = [];
  try {
    files = fs.readdirSync(POLICY_DIR).filter(f => f.endsWith('.json'));
  } catch (err) {
    // Policy directory is optional if baseline policy exists
    if (newHosts.size === 0) {
      console.error(`Failed to read policy directory ${POLICY_DIR}:`, err.message);
      process.exit(1);
    }
    log(`Policy directory ${POLICY_DIR} not found, using baseline only`);
  }

  // Load and merge all policy files
  for (const file of files) {
    const path = `${POLICY_DIR}/${file}`;
    try {
      const policy = JSON.parse(fs.readFileSync(path, 'utf8'));
      const hosts = policy.network?.allow_hosts || [];
      const ips = policy.network?.allow_ips || [];
      const domains = policy.local_domains || [];
      const ports = policy.local_ports || [];

      hosts.forEach(h => newHosts.add(h));
      ips.forEach(ip => newIPs.add(ip));
      domains.forEach(d => newLocalDomains.add(d.toLowerCase()));
      ports.forEach(p => newLocalPorts.add(Number(p)));

      log(`Loaded policy from ${file}: ${hosts.length} hosts, ${ips.length} IPs, ${domains.length} local domains, ${ports.length} local ports`);
    } catch (err) {
      log(`WARNING: Failed to load policy from ${file}: ${err.message}`);
    }
  }

  allowedHosts = newHosts;
  allowedIPs = newIPs;
  localDomains = newLocalDomains;
  localPorts = newLocalPorts;

  return { hosts: newHosts.size, ips: newIPs.size, localDomains: newLocalDomains.size, localPorts: newLocalPorts.size };
}

// Initial load
const counts = loadPolicies();
log(`Loaded ${counts.hosts} hosts, ${counts.ips} IPs, ${counts.localDomains} local domains, ${counts.localPorts} local ports from ${POLICY_DIR}`);
log(`ALLOW_LOCAL=${ALLOW_LOCAL} (private IP ranges ${ALLOW_LOCAL ? 'allowed' : 'blocked'})`);

function log(message) {
  const timestamp = new Date().toISOString();
  const logLine = `[${timestamp}] ${message}`;
  // If LOG_FILE is stdout/stderr, just use console.log to avoid duplicates
  if (LOG_FILE === '/dev/stdout' || LOG_FILE === '/dev/stderr') {
    console.log(logLine);
  } else {
    console.log(logLine);
    try {
      fs.appendFileSync(LOG_FILE, logLine + '\n');
    } catch (err) {
      console.error('Failed to write to log file:', err.message);
    }
  }
}

function isLocalDomain(hostname) {
  const h = hostname.toLowerCase();
  // .local is mDNS/Bonjour local network
  // .localhost is reserved for loopback
  // .internal is commonly used for internal services
  // .lan is commonly used for local networks
  // .home is commonly used for home networks
  return h.endsWith('.local') ||
         h.endsWith('.localhost') ||
         h.endsWith('.internal') ||
         h.endsWith('.lan') ||
         h.endsWith('.home') ||
         h === 'localhost';
}

function isAllowed(hostname, port) {
  const hostPort = `${hostname}:${port}`;
  const portNum = Number(port);

  // Check exact match in allow_hosts
  if (allowedHosts.has(hostPort)) {
    return true;
  }

  // Check IP match in allow_ips
  if (allowedIPs.has(hostname)) {
    return true;
  }

  // Check local_domains from policy (any port on these domains is allowed)
  if (localDomains.has(hostname.toLowerCase())) {
    return true;
  }

  // Check local_ports (these ports are allowed to any local/private IP or local domain)
  if (localPorts.has(portNum) && (isPrivateIP(hostname) || isLocalDomain(hostname))) {
    return true;
  }

  // Allow all traffic to private/local IPs if ALLOW_LOCAL is enabled
  if (ALLOW_LOCAL && isPrivateIP(hostname)) {
    return true;
  }

  // Allow .local, .localhost, .internal, .lan, .home domains and localhost if ALLOW_LOCAL is enabled
  if (ALLOW_LOCAL && isLocalDomain(hostname)) {
    return true;
  }

  // Allow host.containers.internal (podman's host gateway)
  if (ALLOW_LOCAL && hostname === 'host.containers.internal') {
    return true;
  }

  return false;
}

const server = http.createServer((req, res) => {
  const targetUrl = req.url.startsWith('http') ? req.url : `http://${req.headers.host}${req.url}`;
  const parsed = url.parse(targetUrl);
  const hostname = parsed.hostname;
  const port = parsed.port || (parsed.protocol === 'https:' ? 443 : 80);

  if (!isAllowed(hostname, port)) {
    log(`BLOCKED: ${hostname}:${port} - Not in allow list`);
    res.writeHead(403, { 'Content-Type': 'text/plain' });
    res.end(`Access denied: ${hostname}:${port} is not in the allow list\n`);
    return;
  }

  const options = {
    hostname: hostname,
    port: port,
    path: parsed.path,
    method: req.method,
    headers: req.headers
  };

  // Remove proxy-specific headers
  delete options.headers['proxy-connection'];

  const protocol = parsed.protocol === 'https:' ? https : http;

  const proxyReq = protocol.request(options, (proxyRes) => {
    res.writeHead(proxyRes.statusCode, proxyRes.headers);
    proxyRes.pipe(res);
    proxyRes.on('error', () => {
      // Ignore pipe errors after headers sent
    });
  });

  proxyReq.on('error', (err) => {
    log(`ERROR: ${hostname}:${port} - ${err.message}`);
    if (!res.headersSent) {
      res.writeHead(502, { 'Content-Type': 'text/plain' });
      res.end(`Proxy error: ${err.message}\n`);
    }
  });

  req.pipe(proxyReq);
  req.on('error', () => {
    // Ignore client disconnect errors
    proxyReq.destroy();
  });

  // Handle response errors
  res.on('error', () => {
    // Ignore response socket errors
    proxyReq.destroy();
  });
});

// Handle CONNECT for HTTPS
server.on('connect', (req, clientSocket, head) => {
  const [hostname, port] = req.url.split(':');

  if (!isAllowed(hostname, port)) {
    log(`BLOCKED: ${hostname}:${port} - Not in allow list`);
    clientSocket.write('HTTP/1.1 403 Forbidden\r\n\r\n');
    clientSocket.end();
    return;
  }

  const serverSocket = require('net').connect(port, hostname, () => {
    clientSocket.write('HTTP/1.1 200 Connection Established\r\n\r\n');
    serverSocket.write(head);
    serverSocket.pipe(clientSocket);
    clientSocket.pipe(serverSocket);
  });

  serverSocket.on('error', (err) => {
    log(`ERROR: ${hostname}:${port} - ${err.message}`);
    clientSocket.end();
  });

  clientSocket.on('error', () => {
    // Ignore client socket errors
    serverSocket.destroy();
  });
});

// Add reload endpoint
const reloadServer = http.createServer((req, res) => {
  if (req.url === '/reload' && req.method === 'POST') {
    try {
      const counts = loadPolicies();
      log(`Reloaded policies: ${counts.hosts} hosts, ${counts.ips} IPs`);
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ success: true, hosts: counts.hosts, ips: counts.ips }));
    } catch (err) {
      res.writeHead(500, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ success: false, error: err.message }));
    }
  } else if (req.url === '/update-baseline' && req.method === 'POST') {
    // Update baseline policy from POST body and reload
    let body = '';
    req.on('data', chunk => { body += chunk; });
    req.on('end', () => {
      try {
        // Validate JSON
        const policy = JSON.parse(body);
        if (!policy.network || !Array.isArray(policy.network.allow_hosts)) {
          throw new Error('Invalid policy format: requires network.allow_hosts array');
        }
        // Write to baseline file
        fs.writeFileSync(BASELINE_POLICY, JSON.stringify(policy, null, 2));
        log(`Updated baseline policy: ${policy.network.allow_hosts.length} hosts`);
        // Reload all policies
        const counts = loadPolicies();
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({ success: true, hosts: counts.hosts, ips: counts.ips }));
      } catch (err) {
        res.writeHead(400, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({ success: false, error: err.message }));
      }
    });
  } else if (req.url === '/baseline' && req.method === 'GET') {
    // Return current baseline policy
    try {
      const baseline = fs.readFileSync(BASELINE_POLICY, 'utf8');
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(baseline);
    } catch (err) {
      res.writeHead(404, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ error: 'Baseline policy not found' }));
    }
  } else {
    res.writeHead(404);
    res.end();
  }
});

const PORT = process.env.PORT || 8080;
const RELOAD_PORT = process.env.RELOAD_PORT || 8081;

server.listen(PORT, '0.0.0.0', () => {
  log(`Proxy server listening on port ${PORT}`);
});

reloadServer.listen(RELOAD_PORT, '127.0.0.1', () => {
  log(`Reload endpoint listening on localhost:${RELOAD_PORT}`);
});
