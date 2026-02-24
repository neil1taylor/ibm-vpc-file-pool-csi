import type { Config } from 'jest';

const config: Config = {
  testEnvironment: 'jsdom',
  transform: {
    '^.+\\.tsx?$': 'ts-jest',
  },
  moduleNameMapper: {
    // Map the tsconfig path alias
    '^@/(.*)$': '<rootDir>/src/$1',
    // Stub CSS imports
    '\\.css$': '<rootDir>/src/__mocks__/styleMock.ts',
    // Mock the OpenShift console SDK
    '^@openshift-console/dynamic-plugin-sdk$':
      '<rootDir>/src/__mocks__/@openshift-console/dynamic-plugin-sdk.ts',
  },
  setupFilesAfterEnv: ['<rootDir>/src/test-setup.ts'],
  testMatch: ['<rootDir>/src/**/*.test.{ts,tsx}'],
  moduleFileExtensions: ['ts', 'tsx', 'js', 'jsx', 'json'],
};

export default config;
