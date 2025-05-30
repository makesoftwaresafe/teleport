/**
 * Teleport
 * Copyright (C) 2024  Gravitational, Inc.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

import { http, HttpResponse } from 'msw';
import React from 'react';
import { MemoryRouter } from 'react-router';

import { AwsRole } from 'shared/services/apps';

import cfg from 'teleport/config';
import {
  RequiredDiscoverProviders,
  resourceSpecAppAwsCliConsole,
} from 'teleport/Discover/Fixtures/fixtures';
import { getAcl } from 'teleport/mocks/contexts';
import {
  IntegrationKind,
  IntegrationStatusCode,
} from 'teleport/services/integrations';

import { app } from '../fixtures';
import { SetupAccess } from './SetupAccess';

export default {
  title: 'Teleport/Discover/Application/AwsConsole/SetupAccess',
};

const awsRoles: AwsRole[] = [
  {
    name: 'test1',
    display: 'test1',
    arn: 'arn:aws:iam::123456789012:role/test1',
    accountId: '123456789012',
  },
  {
    name: 'test2',
    display: 'test2',
    arn: 'arn:aws:iam::123456789012:role/test2',
    accountId: '123456789012',
  },
];

const defaultUserGet = http.get(cfg.api.userWithUsernamePath, () =>
  HttpResponse.json({
    name: 'user-1',
    roles: [],
    authType: 'local',
    isLocal: true,
    traits: {
      awsRoleArns: [],
    },
    allTraits: {},
  })
);

export const NoTraits = () => (
  <MemoryRouter>
    <Provider awsRoles={[]}>
      <SetupAccess />
    </Provider>
  </MemoryRouter>
);
NoTraits.parameters = {
  msw: {
    handlers: [defaultUserGet],
  },
};

export const WithTraits = () => (
  <MemoryRouter>
    <Provider awsRoles={awsRoles}>
      <SetupAccess />
    </Provider>
  </MemoryRouter>
);
WithTraits.parameters = {
  msw: {
    handlers: [
      http.get(cfg.api.userWithUsernamePath, () =>
        HttpResponse.json({
          name: 'user-1',
          roles: [],
          authType: 'local',
          isLocal: true,
          traits: {
            awsRoleArns: ['arn:aws:iam::123456789012:role/dynamic1'],
          },
          allTraits: {},
        })
      ),
    ],
  },
};

export const NoAccess = () => (
  <MemoryRouter>
    <Provider awsRoles={awsRoles} noAccess={true}>
      <SetupAccess />
    </Provider>
  </MemoryRouter>
);
NoAccess.parameters = {
  msw: {
    handlers: [defaultUserGet],
  },
};

export const SsoUser = () => (
  <MemoryRouter>
    <Provider awsRoles={awsRoles} isSso={true}>
      <SetupAccess />
    </Provider>
  </MemoryRouter>
);
SsoUser.parameters = {
  msw: {
    handlers: [defaultUserGet],
  },
};

const Provider = ({
  children,
  awsRoles,
  noAccess = false,
  isSso = false,
}: {
  children: React.ReactNode;
  awsRoles: AwsRole[];
  noAccess?: boolean;
  isSso?: boolean;
}) => {
  return (
    <RequiredDiscoverProviders
      agentMeta={{
        app: {
          ...app,
          awsRoles,
        },
        awsIntegration: {
          resourceType: 'integration',
          kind: IntegrationKind.AwsOidc,
          name: 'some-aws-oidc-name',
          statusCode: IntegrationStatusCode.Running,
          spec: {
            roleArn: 'arn:aws:iam::123456789012:role/some-iam-role-name',
            issuerS3Bucket: '',
            issuerS3Prefix: '',
          },
        },
      }}
      resourceSpec={resourceSpecAppAwsCliConsole}
      authType={isSso ? 'sso' : undefined}
      customAcl={noAccess ? getAcl({ noAccess: true }) : undefined}
    >
      {children}
    </RequiredDiscoverProviders>
  );
};
