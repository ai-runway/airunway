import app from './hono-app';
import logger from './lib/logger';
import { authService } from './services/auth';
import { readFileSync } from 'fs';
import { addKeepResourcePolicyToCrdManifest } from './services/helm';

const PORT = process.env.PORT || 3001;

/**
 * CLI command handlers
 */
async function handleLogin(args: string[]): Promise<void> {
  // Parse arguments
  let serverUrl = `http://localhost:${PORT}`;
  let contextName: string | undefined;
  
  for (let i = 0; i < args.length; i++) {
    if (args[i] === '--server' || args[i] === '-s') {
      serverUrl = args[i + 1] || serverUrl;
      i++;
    } else if (args[i] === '--context' || args[i] === '-c') {
      contextName = args[i + 1];
      i++;
    }
  }

  console.log('🔐 AI Runway Login\n');
  console.log('Extracting credentials from kubeconfig...');

  const credentials = await authService.extractTokenFromKubeconfig(contextName);
  
  if (!credentials) {
    console.error('\n❌ Failed to extract token from kubeconfig.');
    console.error('\nMake sure your kubeconfig has valid OIDC or token-based authentication.');
    console.error('Try running: kubectl get pods  (to refresh your credentials)');
    process.exit(1);
  }

  // Save credentials locally
  authService.saveCredentials(credentials);

  // Generate login URL
  const loginUrl = authService.generateLoginUrl(serverUrl, credentials.token);

  console.log(`\n✅ Logged in as: ${credentials.username}`);
  if (credentials.expiresAt) {
    console.log(`   Token expires: ${new Date(credentials.expiresAt).toLocaleString()}`);
  }

  console.log('\n📋 Open this URL in your browser to authenticate:\n');
  console.log(`   ${loginUrl}\n`);

  // Try to open browser automatically
  try {
    const openCommand = process.platform === 'darwin' ? 'open' : 
                        process.platform === 'win32' ? 'start' : 'xdg-open';
    
    const proc = Bun.spawn([openCommand, loginUrl], {
      stdout: 'ignore',
      stderr: 'ignore',
    });
    await proc.exited;
    console.log('   (Browser opened automatically)');
  } catch {
    console.log('   (Copy and paste the URL above into your browser)');
  }
}

function handleLogout(): void {
  authService.clearCredentials();
  console.log('✅ Logged out. Credentials cleared.');
}

function handleVersion(): void {
  console.log('AI Runway v1.0.0');
}

function printUsage(): void {
  console.log(`
AI Runway - ML Model Deployment Platform

Usage: airunway <command> [options]

Commands:
  serve              Start the AI Runway server (default)
  login              Authenticate using kubeconfig credentials
  logout             Clear stored credentials
  version            Show version information

Login Options:
  --server, -s       Server URL (default: http://localhost:${PORT})
  --context, -c      Kubeconfig context to use (default: current context)

Examples:
  airunway                         # Start server
  airunway serve                   # Start server
  airunway login                   # Login with current context
  airunway login --context myaks   # Login with specific context
`);
}

async function startServer(): Promise<void> {
  const server = Bun.serve({
    port: Number(PORT),
    fetch: app.fetch,
    // Increase idle timeout for long-running operations like Helm installs
    // Max value is 255 seconds (~4 minutes)
    idleTimeout: 255,
  });

  const authEnabled = authService.isAuthEnabled();
  
  logger.info({ port: server.port, authEnabled }, `🚀 AI Runway backend running on http://localhost:${server.port}`);
  
  if (authEnabled) {
    console.log('\n🔐 Authentication is ENABLED');
    console.log(`   Run: airunway login --server http://localhost:${server.port}\n`);
  }
}

// Main CLI entry point
async function main(): Promise<void> {
  const args = process.argv.slice(2);
  const command = args[0] || 'serve';

  switch (command) {
    case 'serve':
      await startServer();
      break;
    case 'login':
      await handleLogin(args.slice(1));
      break;
    case 'logout':
      handleLogout();
      break;
    case 'version':
    case '--version':
    case '-v':
      handleVersion();
      break;
    case 'help':
    case '--help':
    case '-h':
      printUsage();
      break;
    default:
      // If no recognized command, assume it's serve
      if (command.startsWith('-')) {
        await startServer();
      } else {
        console.error(`Unknown command: ${command}`);
        printUsage();
        process.exit(1);
      }
  }
}

if (process.env.AIRUNWAY_HELM_POST_RENDERER === 'keep-crd-resources') {
  const renderedManifest = addKeepResourcePolicyToCrdManifest(readFileSync(0, 'utf8'));
  process.stdout.write(renderedManifest, (error) => {
    if (error) {
      process.exit(1);
    }
    process.exit(0);
  });
} else {
  main().catch((error) => {
    logger.error({ error }, 'Fatal error');
    process.exit(1);
  });
}
