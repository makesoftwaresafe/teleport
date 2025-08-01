/**
 * Teleport
 * Copyright (C) 2025 Gravitational, Inc.
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

import { QueryClientProvider } from '@tanstack/react-query';
import { setupServer } from 'msw/node';
import { PropsWithChildren } from 'react';
import { MemoryRouter } from 'react-router';

import { darkTheme } from 'design/theme';
import { ConfiguredThemeProvider } from 'design/ThemeProvider';
import {
  fireEvent,
  render,
  screen,
  testQueryClient,
  userEvent,
  waitFor,
  waitForElementToBeRemoved,
} from 'design/utils/testing';
import { InfoGuidePanelProvider } from 'shared/components/SlidingSidePanel/InfoGuide';

import { createTeleportContext } from 'teleport/mocks/contexts';
import { listBotInstances } from 'teleport/services/bot/bot';
import { makeAcl } from 'teleport/services/user/makeAcl';
import {
  listBotInstancesError,
  listBotInstancesSuccess,
} from 'teleport/test/helpers/botInstances';

import { ContextProvider } from '..';
import { BotInstances } from './BotInstances';

jest.mock('teleport/services/bot/bot', () => {
  const actual = jest.requireActual('teleport/services/bot/bot');
  return {
    listBotInstances: jest.fn((...all) => {
      return actual.listBotInstances(...all);
    }),
  };
});

const server = setupServer();

beforeAll(() => {
  server.listen();
});

beforeEach(() => {
  jest.useFakeTimers().setSystemTime(new Date('2025-05-19T08:00:00Z'));
});

afterEach(async () => {
  server.resetHandlers();
  await testQueryClient.resetQueries();

  jest.useRealTimers();
  jest.clearAllMocks();
});

afterAll(() => server.close());

describe('BotInstances', () => {
  it('Shows an empty state', async () => {
    server.use(
      listBotInstancesSuccess({
        bot_instances: [],
        next_page_token: '',
      })
    );

    render(<BotInstances />, { wrapper: makeWrapper() });

    await waitForElementToBeRemoved(() => screen.queryByTestId('loading'));

    expect(screen.getByText('No active instances found')).toBeInTheDocument();
    expect(
      screen.getByText(
        'Bot instances are ephemeral, and disappear once all issued credentials have expired.'
      )
    ).toBeInTheDocument();
  });

  it('Shows an error state', async () => {
    server.use(listBotInstancesError(500, 'server error'));

    render(<BotInstances />, { wrapper: makeWrapper() });

    await waitForElementToBeRemoved(() => screen.queryByTestId('loading'));

    expect(screen.getByText('Error: server error')).toBeInTheDocument();
  });

  it('Shows an unsupported sort error state', async () => {
    const testErrorMessage =
      'unsupported sort, only bot_name:asc is supported, but got "blah" (desc = true)';
    server.use(listBotInstancesError(400, testErrorMessage));

    render(<BotInstances />, { wrapper: makeWrapper() });

    await waitForElementToBeRemoved(() => screen.queryByTestId('loading'));

    expect(screen.getByText(`Error: ${testErrorMessage}`)).toBeInTheDocument();

    server.use(
      listBotInstancesSuccess({
        bot_instances: [],
        next_page_token: '',
      })
    );

    const resetButton = screen.getByText('Reset sort');
    expect(resetButton).toBeInTheDocument();
    fireEvent.click(resetButton);

    await waitForElementToBeRemoved(() => screen.queryByTestId('loading'));

    expect(
      screen.queryByText(`Error: ${testErrorMessage}`)
    ).not.toBeInTheDocument();
  });

  it('Shows an unauthorised error state', async () => {
    render(<BotInstances />, {
      wrapper: makeWrapper(
        makeAcl({
          botInstances: {
            list: false,
            create: true,
            edit: true,
            remove: true,
            read: true,
          },
        })
      ),
    });

    expect(
      screen.getByText(
        'You do not have permission to access Bot instances. Missing role permissions:',
        { exact: false }
      )
    ).toBeInTheDocument();

    expect(screen.getByText('bot_instance.list')).toBeInTheDocument();
  });

  it('Shows a list', async () => {
    server.use(
      listBotInstancesSuccess({
        bot_instances: [
          {
            bot_name: 'test-bot-1',
            instance_id: '5e885c66-1af3-4a36-987d-a604d8ee49d2',
            active_at_latest: '2025-05-19T07:32:00Z',
            host_name_latest: 'test-hostname',
            join_method_latest: 'test-join-method',
            version_latest: '1.0.0-dev-a12b3c',
          },
          {
            bot_name: 'test-bot-2',
            instance_id: '3c3aae3e-de25-4824-a8e9-5a531862f19a',
          },
        ],
        next_page_token: '',
      })
    );

    render(<BotInstances />, { wrapper: makeWrapper() });

    await waitForElementToBeRemoved(() => screen.queryByTestId('loading'));

    expect(screen.getByText('test-bot-1')).toBeInTheDocument();
    expect(screen.getByText('5e885c6')).toBeInTheDocument();
    expect(screen.getByText('28 minutes ago')).toBeInTheDocument();
    expect(screen.getByText('test-hostname')).toBeInTheDocument();
    expect(screen.getByText('test-join-method')).toBeInTheDocument();
    expect(screen.getByText('v1.0.0-dev-a12b3c')).toBeInTheDocument();
  });

  it('Allows paging', async () => {
    jest.mocked(listBotInstances).mockImplementation(
      ({ pageToken }) =>
        new Promise(resolve => {
          resolve({
            bot_instances: [
              {
                bot_name: `test-bot`,
                instance_id: `00000000-0000-4000-0000-000000000000`,
                active_at_latest: `2025-05-19T07:32:00Z`,
                host_name_latest: 'test-hostname',
                join_method_latest: 'test-join-method',
                version_latest: `1.0.0-dev-a12b3c`,
              },
            ],
            next_page_token: pageToken + '.next',
          });
        })
    );

    expect(listBotInstances).toHaveBeenCalledTimes(0);

    render(<BotInstances />, { wrapper: makeWrapper() });

    await waitForElementToBeRemoved(() => screen.queryByTestId('loading'));

    const [nextButton] = screen.getAllByTitle('Next page');

    expect(listBotInstances).toHaveBeenCalledTimes(1);
    expect(listBotInstances).toHaveBeenLastCalledWith({
      pageSize: 20,
      pageToken: '',
      searchTerm: '',
      sort: 'active_at_latest:desc',
    });

    await waitFor(() => expect(nextButton).toBeEnabled());
    fireEvent.click(nextButton);

    expect(listBotInstances).toHaveBeenCalledTimes(2);
    expect(listBotInstances).toHaveBeenLastCalledWith({
      pageSize: 20,
      pageToken: '.next',
      searchTerm: '',
      sort: 'active_at_latest:desc',
    });

    await waitFor(() => expect(nextButton).toBeEnabled());
    fireEvent.click(nextButton);

    expect(listBotInstances).toHaveBeenCalledTimes(3);
    expect(listBotInstances).toHaveBeenLastCalledWith({
      pageSize: 20,
      pageToken: '.next.next',
      searchTerm: '',
      sort: 'active_at_latest:desc',
    });

    const [prevButton] = screen.getAllByTitle('Previous page');

    await waitFor(() => expect(prevButton).toBeEnabled());
    fireEvent.click(prevButton);

    // This page's data will have been cached
    expect(listBotInstances).toHaveBeenCalledTimes(3);

    await waitFor(() => expect(prevButton).toBeEnabled());
    fireEvent.click(prevButton);

    // This page's data will have been cached
    expect(listBotInstances).toHaveBeenCalledTimes(3);
  });

  it('Allows filtering (search)', async () => {
    jest.mocked(listBotInstances).mockImplementation(
      ({ pageToken }) =>
        new Promise(resolve => {
          resolve({
            bot_instances: [
              {
                bot_name: `test-bot`,
                instance_id: `00000000-0000-4000-0000-000000000000`,
                active_at_latest: `2025-05-19T07:32:00Z`,
                host_name_latest: 'test-hostname',
                join_method_latest: 'test-join-method',
                version_latest: `1.0.0-dev-a12b3c`,
              },
            ],
            next_page_token: pageToken + '.next',
          });
        })
    );

    expect(listBotInstances).toHaveBeenCalledTimes(0);

    render(<BotInstances />, { wrapper: makeWrapper() });

    await waitForElementToBeRemoved(() => screen.queryByTestId('loading'));

    expect(listBotInstances).toHaveBeenCalledTimes(1);
    expect(listBotInstances).toHaveBeenLastCalledWith({
      pageSize: 20,
      pageToken: '',
      searchTerm: '',
      sort: 'active_at_latest:desc',
    });

    const [nextButton] = screen.getAllByTitle('Next page');
    await waitFor(() => expect(nextButton).toBeEnabled());
    fireEvent.click(nextButton);

    expect(listBotInstances).toHaveBeenCalledTimes(2);
    expect(listBotInstances).toHaveBeenLastCalledWith({
      pageSize: 20,
      pageToken: '.next',
      searchTerm: '',
      sort: 'active_at_latest:desc',
    });

    jest.useRealTimers(); // Required as userEvent.type() uses setTimeout internally

    const search = screen.getByPlaceholderText('Search...');
    await waitFor(() => expect(search).toBeEnabled());
    await userEvent.type(search, 'test-search-term');
    await userEvent.type(search, '{enter}');

    expect(listBotInstances).toHaveBeenCalledTimes(3);
    expect(listBotInstances).toHaveBeenLastCalledWith({
      pageSize: 20,
      pageToken: '', // Search should reset to the first page
      searchTerm: 'test-search-term',
      sort: 'active_at_latest:desc',
    });
  });

  it('Allows sorting', async () => {
    jest.mocked(listBotInstances).mockImplementation(
      ({ pageToken }) =>
        new Promise(resolve => {
          resolve({
            bot_instances: [
              {
                bot_name: `test-bot`,
                instance_id: `00000000-0000-4000-0000-000000000000`,
                active_at_latest: `2025-05-19T07:32:00Z`,
                host_name_latest: 'test-hostname',
                join_method_latest: 'test-join-method',
                version_latest: `1.0.0-dev-a12b3c`,
              },
            ],
            next_page_token: pageToken + '.next',
          });
        })
    );

    expect(listBotInstances).toHaveBeenCalledTimes(0);

    render(<BotInstances />, { wrapper: makeWrapper() });

    await waitForElementToBeRemoved(() => screen.queryByTestId('loading'));

    const lastHeartbeatHeader = screen.getByText('Last heartbeat');

    expect(listBotInstances).toHaveBeenCalledTimes(1);
    expect(listBotInstances).toHaveBeenLastCalledWith({
      pageSize: 20,
      pageToken: '',
      searchTerm: '',
      sort: 'active_at_latest:desc',
    });

    fireEvent.click(lastHeartbeatHeader);

    expect(listBotInstances).toHaveBeenCalledTimes(2);
    expect(listBotInstances).toHaveBeenLastCalledWith({
      pageSize: 20,
      pageToken: '',
      searchTerm: '',
      sort: 'active_at_latest:asc',
    });

    const botHeader = screen.getByText('Bot');
    fireEvent.click(botHeader);

    expect(listBotInstances).toHaveBeenCalledTimes(3);
    expect(listBotInstances).toHaveBeenLastCalledWith({
      pageSize: 20,
      pageToken: '',
      searchTerm: '',
      sort: 'bot_name:desc',
    });
  });
});

function makeWrapper(
  customAcl: ReturnType<typeof makeAcl> = makeAcl({
    botInstances: {
      list: true,
      create: true,
      edit: true,
      remove: true,
      read: true,
    },
  })
) {
  return ({ children }: PropsWithChildren) => {
    const ctx = createTeleportContext({
      customAcl,
    });
    return (
      <MemoryRouter>
        <QueryClientProvider client={testQueryClient}>
          <ConfiguredThemeProvider theme={darkTheme}>
            <InfoGuidePanelProvider data-testid="blah">
              <ContextProvider ctx={ctx}>{children}</ContextProvider>
            </InfoGuidePanelProvider>
          </ConfiguredThemeProvider>
        </QueryClientProvider>
      </MemoryRouter>
    );
  };
}
