import type { UserInfo } from '@airunway/shared';

export type AppEnv = {
  Variables: {
    user?: UserInfo;
    token?: string;
  };
};
