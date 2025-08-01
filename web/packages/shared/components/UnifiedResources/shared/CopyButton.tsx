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

import { MouseEventHandler, useEffect, useRef, useState } from 'react';

import Box from 'design/Box';
import ButtonIcon from 'design/ButtonIcon';
import { Check, Copy } from 'design/Icon';
import { HoverTooltip } from 'design/Tooltip';
import { copyToClipboard } from 'design/utils/copyToClipboard';

export function CopyButton({
  name,
  mr,
  ml,
}: {
  name: string;
  mr?: number;
  ml?: number;
}) {
  const copySuccess = 'Copied!';
  const copyDefault = 'Click to copy';
  const timeout = useRef<ReturnType<typeof setTimeout>>(undefined);
  const copyAnchorEl = useRef(null);
  const [copiedText, setCopiedText] = useState(copyDefault);

  const clearCurrentTimeout = () => {
    if (timeout.current) {
      clearTimeout(timeout.current);
      timeout.current = undefined;
    }
  };

  const handleCopy: MouseEventHandler<unknown> = e => {
    e.stopPropagation(); // Prevent parent onClick callbacks from stealing the click

    clearCurrentTimeout();
    setCopiedText(copySuccess);
    copyToClipboard(name);
    // Change to default text after 1 second
    timeout.current = setTimeout(() => {
      setCopiedText(copyDefault);
    }, 1000);
  };

  useEffect(() => {
    return () => clearCurrentTimeout();
  }, []);

  return (
    <Box mr={mr} ml={ml}>
      <HoverTooltip tipContent={copiedText}>
        <ButtonIcon
          ref={copyAnchorEl}
          size={0}
          onClick={handleCopy}
          aria-label="copy"
        >
          {copiedText === copySuccess ? (
            <Check size="small" />
          ) : (
            <Copy size="small" />
          )}
        </ButtonIcon>
      </HoverTooltip>
    </Box>
  );
}
