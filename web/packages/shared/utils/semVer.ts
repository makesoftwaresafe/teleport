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

import compare from 'semver/functions/compare';
import major from 'semver/functions/major';
import parse from 'semver/functions/parse';

// Re-export specific functions from `semver/functions/*` to avoid bundling
// the full semver package.

export {
  compare,
  /** @deprecated Import `compare` from `shared/utils/semver`. */
  compare as compareSemVers,
  major,
  parse,
};
