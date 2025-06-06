/**
 * Teleport
 * Copyright (C) 2023  Gravitational, Inc.
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

import { MemoryRouter } from 'react-router';

import { ListThin } from 'design/Icon';
import { fireEvent, render, screen } from 'design/utils/testing';
import { InfoGuideButton } from 'shared/components/SlidingSidePanel/InfoGuide/InfoGuide';

import { Context, ContextProvider } from 'teleport';
import { apps } from 'teleport/Apps/fixtures';
import { events } from 'teleport/Audit/fixtures';
import { clusters } from 'teleport/Clusters/fixtures';
import { databases } from 'teleport/Databases/fixtures';
import { desktops } from 'teleport/Desktops/fixtures';
import { getOSSFeatures } from 'teleport/features';
import { kubes } from 'teleport/Kubes/fixtures';
import { userContext } from 'teleport/Main/fixtures';
import { LayoutContextProvider } from 'teleport/Main/LayoutContext';
import { createTeleportContext } from 'teleport/mocks/contexts';
import { NavigationCategory } from 'teleport/Navigation';
import { nodes } from 'teleport/Nodes/fixtures';
import { sessions } from 'teleport/Sessions/fixtures';
import TeleportContext from 'teleport/teleportContext';
import { TeleportFeature } from 'teleport/types';
import { makeTestUserContext } from 'teleport/User/testHelpers/makeTestUserContext';
import { mockUserContextProviderWith } from 'teleport/User/testHelpers/mockUserContextWith';

import { Main, MainProps } from './Main';

const setupContext = (): TeleportContext => {
  const ctx = new Context();
  ctx.isEnterprise = false;
  ctx.auditService.fetchEvents = () =>
    Promise.resolve({ events, startKey: '' });
  ctx.clusterService.fetchClusters = () => Promise.resolve(clusters);
  ctx.nodeService.fetchNodes = () => Promise.resolve({ agents: nodes });
  ctx.sshService.fetchSessions = () => Promise.resolve(sessions);
  ctx.appService.fetchApps = () => Promise.resolve({ agents: apps });
  ctx.kubeService.fetchKubernetes = () => Promise.resolve({ agents: kubes });
  ctx.databaseService.fetchDatabases = () =>
    Promise.resolve({ agents: databases });
  ctx.desktopService.fetchDesktops = () =>
    Promise.resolve({ agents: desktops });
  ctx.storeUser.setState(userContext);

  return ctx;
};

test('renders', () => {
  mockUserContextProviderWith(makeTestUserContext());
  const ctx = setupContext();

  const props: MainProps = {
    features: getOSSFeatures(),
  };

  render(
    <MemoryRouter>
      <LayoutContextProvider>
        <ContextProvider ctx={ctx}>
          <Main {...props} />
        </ContextProvider>
      </LayoutContextProvider>
    </MemoryRouter>
  );

  expect(screen.getByTestId('teleport-logo')).toBeInTheDocument();
});

test('toggle rendering of info guide panel', async () => {
  mockUserContextProviderWith(makeTestUserContext());
  const ctx = createTeleportContext();

  const props: MainProps = {
    features: [...getOSSFeatures(), new FeatureTestInfoGuide()],
  };

  render(
    <MemoryRouter>
      <ContextProvider ctx={ctx}>
        <LayoutContextProvider>
          <Main {...props} />
        </LayoutContextProvider>
      </ContextProvider>
    </MemoryRouter>
  );

  expect(screen.getByTestId('teleport-logo')).toBeInTheDocument();

  expect(screen.queryByText(/i am the guide/i)).not.toBeInTheDocument();
  expect(screen.queryByText(/info guide title/i)).not.toBeInTheDocument();

  // render the component that has the guide info button
  fireEvent.click(screen.queryAllByText('Zero Trust Access')[0]);
  fireEvent.click(screen.getByText(/test info guide/i));
  expect(screen.getByText(/info guide title/i)).toBeInTheDocument();

  // test opening of panel
  fireEvent.click(screen.getByTestId('info-guide-btn-open'));
  expect(screen.getByText(/i am the guide/i)).toBeInTheDocument();

  // test closing of panel by clicking on explicit close button
  fireEvent.click(screen.getByTestId('info-guide-btn-close'));
  expect(screen.queryByText(/i am the guide/i)).not.toBeInTheDocument();
});

test('displays invite collaborators feedback if present', () => {
  mockUserContextProviderWith(makeTestUserContext());
  const ctx = setupContext();

  const props: MainProps = {
    features: getOSSFeatures(),
    inviteCollaboratorsFeedback: <div>Passed Component!</div>,
  };

  render(
    <MemoryRouter>
      <LayoutContextProvider>
        <ContextProvider ctx={ctx}>
          <Main {...props} />
        </ContextProvider>
      </LayoutContextProvider>
    </MemoryRouter>
  );

  expect(screen.getByText('Passed Component!')).toBeInTheDocument();
});

test('renders without invite collaborators feedback enabled', () => {
  mockUserContextProviderWith(makeTestUserContext());
  const ctx = setupContext();

  const props: MainProps = {
    features: getOSSFeatures(),
  };
  expect(props.inviteCollaboratorsFeedback).toBeUndefined();

  render(
    <MemoryRouter>
      <LayoutContextProvider>
        <ContextProvider ctx={ctx}>
          <Main {...props} />
        </ContextProvider>
      </LayoutContextProvider>
    </MemoryRouter>
  );

  expect(screen.getByTestId('teleport-logo')).toBeInTheDocument();
});

const TestInfoGuide = () => {
  return (
    <div>
      <InfoGuideButton config={{ guide: <div>I am the guide</div> }}>
        Info Guide Title
      </InfoGuideButton>
    </div>
  );
};

class FeatureTestInfoGuide implements TeleportFeature {
  category = NavigationCategory.Audit;

  route = {
    title: 'Test Info Guide',
    path: '/web/testinfoguide',
    component: TestInfoGuide,
  };

  navigationItem = {
    title: 'Test Info Guide' as any,
    icon: ListThin,
    getLink() {
      return '/web/testinfoguide';
    },
    searchableTags: ['test info guide'],
  };

  hasAccess() {
    return true;
  }
}
