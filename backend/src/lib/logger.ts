import pino from 'pino';

// Use simple JSON logging in production/compiled mode
// pino-pretty transport doesn't work in compiled binaries
export const logger = pino({
  level: process.env.LOG_LEVEL || 'info',
  formatters: {
    level: (label) => ({ level: label }),
  },
  timestamp: () => `,"time":"${new Date().toISOString()}"`,
});

export default logger;
