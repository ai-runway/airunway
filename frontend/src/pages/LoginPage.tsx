import { useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useAuth } from '@/hooks/useAuth';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';

export function LoginPage() {
  const navigate = useNavigate();
  const { isAuthenticated, isLoading, error, login, checkTokenFromUrl } = useAuth();
  const [manualToken, setManualToken] = useState('');
  const [showManualInput, setShowManualInput] = useState(false);

  // Check for token in URL on mount
  useEffect(() => {
    const foundToken = checkTokenFromUrl();
    if (foundToken) {
      // Token was found and processed, redirect will happen via isAuthenticated
    }
  }, [checkTokenFromUrl]);

  // Redirect when authenticated
  useEffect(() => {
    if (isAuthenticated && !isLoading) {
      navigate('/', { replace: true });
    }
  }, [isAuthenticated, isLoading, navigate]);

  const handleManualLogin = () => {
    if (manualToken.trim()) {
      login(manualToken.trim());
    }
  };

  const handleRefresh = () => {
    window.location.reload();
  };

  if (isLoading) {
    return (
      <div className="min-h-screen flex items-center justify-center bg-background">
        <div className="text-center">
          <div className="animate-spin rounded-full h-8 w-8 border-b-2 border-primary mx-auto mb-4" />
          <p className="text-muted-foreground">Checking authentication...</p>
        </div>
      </div>
    );
  }

  if (isAuthenticated) {
    return (
      <div className="min-h-screen flex items-center justify-center bg-background">
        <div className="text-center">
          <p className="text-muted-foreground">Redirecting...</p>
        </div>
      </div>
    );
  }

  return (
    <div className="min-h-screen flex items-center justify-center bg-background p-4">
      <Card className="w-full max-w-md">
        <CardHeader className="text-center">
          <div className="mx-auto mb-4 h-12 w-12 rounded-lg bg-primary flex items-center justify-center">
            <svg
              xmlns="http://www.w3.org/2000/svg"
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              strokeWidth="2"
              strokeLinecap="round"
              strokeLinejoin="round"
              className="h-6 w-6 text-primary-foreground"
            >
              <path d="M12 2L2 7l10 5 10-5-10-5z" />
              <path d="M2 17l10 5 10-5" />
              <path d="M2 12l10 5 10-5" />
            </svg>
          </div>
          <CardTitle className="text-2xl">AIRunway</CardTitle>
          <CardDescription>Authentication Required</CardDescription>
        </CardHeader>
        <CardContent className="space-y-6">
          {error && (
            <div className="rounded-lg bg-destructive/10 p-3 text-sm text-destructive">
              {error}
            </div>
          )}

          {/* CLI Login Instructions */}
          <div className="space-y-3">
            <p className="text-sm text-muted-foreground">
              Run this command in your terminal to authenticate:
            </p>
            <div className="rounded-lg bg-muted p-3 font-mono text-sm">
              <code>airunway login</code>
            </div>
            <p className="text-xs text-muted-foreground">
              This will extract your credentials and open this page automatically.
            </p>
          </div>

          <div className="flex items-center gap-4">
            <div className="h-px flex-1 bg-border" />
            <span className="text-xs text-muted-foreground uppercase">or</span>
            <div className="h-px flex-1 bg-border" />
          </div>

          {/* Manual Token Input */}
          {showManualInput ? (
            <div className="space-y-3">
              <div className="space-y-2">
                <Label htmlFor="token">Paste Token Manually</Label>
                <Input
                  id="token"
                  type="password"
                  placeholder="eyJhbG..."
                  value={manualToken}
                  onChange={(e) => setManualToken(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter') handleManualLogin();
                  }}
                />
              </div>
              <div className="flex gap-2">
                <Button onClick={handleManualLogin} className="flex-1">
                  Login
                </Button>
                <Button variant="outline" onClick={() => setShowManualInput(false)}>
                  Cancel
                </Button>
              </div>
            </div>
          ) : (
            <div className="flex gap-2">
              <Button variant="outline" onClick={handleRefresh} className="flex-1">
                Check Again
              </Button>
              <Button variant="ghost" onClick={() => setShowManualInput(true)}>
                Paste Token
              </Button>
            </div>
          )}

          {/* Help text */}
          <div className="text-center text-xs text-muted-foreground">
            <p>
              Need help?{' '}
              <a
                href="https://github.com/ai-runway/airunway#authentication"
                target="_blank"
                rel="noopener noreferrer"
                className="text-primary hover:underline"
              >
                View documentation
              </a>
            </p>
          </div>
        </CardContent>
      </Card>
    </div>
  );
}
