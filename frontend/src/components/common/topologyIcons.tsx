// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import * as React from 'react';
import { useEffect, useMemo } from 'react';
import * as THREE from 'three';

const gatewayPoolSvgIcon = `
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 572 42.22 40" preserveAspectRatio="xMidYMid meet">
  <style>
    .st1{fill:#00bbf1;stroke:none;stroke-linecap:round;stroke-linejoin:round;stroke-width:.75}
    .st2{fill:#919191;stroke:none;stroke-linecap:round;stroke-linejoin:round;stroke-width:.75}
    .st3{fill:#c5c5c5;stroke:none;stroke-linecap:round;stroke-linejoin:round;stroke-width:.75}
    .st4{fill:#ffffff;stroke:none;stroke-linecap:round;stroke-linejoin:round;stroke-width:.75}
  </style>
  <g>
    <g transform="translate(2.95521,-10.3094)"><path d="M37.33 585.68 L37.33 612 L0 612 L0 585.68 L37.39 585.59 L37.33 585.68 L37.33 585.68 Z" class="st1"/></g>
    <g transform="translate(7.17693,0)"><path d="M20.26 603.97 C19.21 603.97 19.21 603.97 19.21 603.97 C9.24 603.97 9.24 603.97 9.24 603.97 C8.68 603.97 8.68 603.97 8.68 603.97 C10.05 608.8 8.18 609.52 0 609.52 C0 612 0 612 0 612 C10.48 612 10.48 612 10.48 612 C18.07 612 18.07 612 18.07 612 C27.86 612 27.86 612 27.86 612 C27.86 609.52 27.86 609.52 27.86 609.52 C19.71 609.52 18.89 608.8 20.26 603.97 Z" class="st2"/></g>
    <g transform="translate(0,-8.02822)"><path d="M39.63 580.73 C2.37 580.73 2.37 580.73 2.37 580.73 C1.06 580.73 0 581.89 0 583.21 C0 609.62 0 609.62 0 609.62 C0 610.94 1.06 612 2.37 612 C39.63 612 39.63 612 39.63 612 C40.92 612 42.22 610.94 42.22 609.62 C42.22 583.21 42.22 583.21 42.22 583.21 C42.22 581.89 40.92 580.73 39.63 580.73 ZM39 584.03 C39 608.73 39 608.73 39 608.73 C3.25 608.73 3.25 608.73 3.25 608.73 C3.25 584.03 3.25 584.03 3.25 584.03 C39.08 583.95 39.08 583.95 39.08 583.95 L39 584.03 Z" class="st3"/></g>
    <g transform="translate(6.48363,-15.7488)">
      <g transform="translate(20.3825,0)"><path d="M8.87 604.07 L7.13 602.34 L1.01 596.33 L0 597.24 L6.95 604.07 L0 611.09 L1.01 612 L7.13 605.98 L8.87 604.07 L8.87 604.07 Z" class="st4"/></g>
      <g><path d="M0 604.07 L1.73 602.34 L7.86 596.33 L8.87 597.24 L1.92 604.07 L8.87 611.09 L7.86 612 L1.73 605.98 L0 604.07 L0 604.07 Z" class="st4"/></g>
      <g transform="translate(7.4917,-6.1882)"><path d="M3.57 610.26 C3.57 611.26 2.75 612 1.83 612 C0.91 612 0 611.17 0 610.26 C-0 609.35 0.74 608.53 1.83 608.53 C2.83 608.53 3.57 609.35 3.57 610.26 Z" class="st4"/></g>
      <g transform="translate(12.7983,-6.1882)"><path d="M3.56 610.26 C3.56 611.26 2.74 612 1.83 612 C0.91 612 -0 611.17 0 610.26 C0 609.35 0.82 608.53 1.83 608.53 C2.83 608.53 3.56 609.35 3.56 610.26 Z" class="st4"/></g>
      <g transform="translate(18.2784,-6.1882)"><path d="M3.48 610.26 C3.48 609.3 2.69 608.53 1.73 608.53 C0.79 608.53 -0 609.3 0 610.26 C0 611.22 0.79 612 1.73 612 C2.69 612 3.48 611.22 3.48 610.26 Z" class="st4"/></g>
    </g>
  </g>
</svg>`;

const workerSiteSvgIcon = `
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 81 103.336" preserveAspectRatio="xMidYMid meet">
  <style>
    .st1{fill:#00bbf1;stroke:none;stroke-linecap:round;stroke-linejoin:round;stroke-width:.75}
    .st2{fill:#c5c5c5;stroke:none;stroke-linecap:round;stroke-linejoin:round;stroke-width:.75}
    .st3{fill:#e6f8fe;stroke:none;stroke-linecap:round;stroke-linejoin:round;stroke-width:.75}
    .st4{fill:#ccf1fc;stroke:none;stroke-linecap:round;stroke-linejoin:round;stroke-width:.75}
    .st5{fill:#80ddf8;stroke:none;stroke-linecap:round;stroke-linejoin:round;stroke-width:.75}
    .st6{fill:#919191;stroke:none;stroke-linecap:round;stroke-linejoin:round;stroke-width:.75}
  </style>
  <g transform="translate(0,-22.4)">
    <g transform="translate(0,-36.5878)">
      <g transform="translate(4.19152,-3.23548)"><path d="M52.95 66 L52.95 103.34 L0 103.34 L0 66 L53.03 65.88 L52.95 66 L52.95 66 Z" class="st1"/></g>
      <g><path d="M56.21 58.99 C3.37 58.99 3.37 58.99 3.37 58.99 C1.5 58.99 -0 60.64 0 62.51 C0 99.97 0 99.97 0 99.97 C-0 101.84 1.5 103.34 3.37 103.34 C56.21 103.34 56.21 103.34 56.21 103.34 C58.05 103.34 59.88 101.84 59.88 99.97 C59.88 62.51 59.88 62.51 59.88 62.51 C59.88 60.64 58.05 58.99 56.21 58.99 ZM55.31 63.67 C55.31 98.69 55.31 98.69 55.31 98.69 C4.6 98.69 4.6 98.69 4.6 98.69 C4.6 63.67 4.6 63.67 4.6 63.67 C55.43 63.56 55.43 63.56 55.43 63.56 L55.31 63.67 Z" class="st2"/></g>
      <g transform="translate(19.1612,-22.7736)"><path d="M10.78 90.75 C10.67 90.75 10.67 90.75 10.55 90.75 C0.15 96.78 0.15 96.78 0.15 96.78 C-0 96.78 0 96.89 0 97.01 C0 97.12 -0 97.23 0.15 97.34 C10.67 103.34 10.67 103.34 10.67 103.34 C10.67 103.34 10.78 103.34 10.78 103.34 C10.93 103.34 10.93 103.34 11.04 103.34 C21.44 97.34 21.44 97.34 21.44 97.34 C21.56 97.34 21.56 97.23 21.56 97.12 C21.56 97.01 21.56 96.89 21.44 96.78 C10.93 90.75 10.93 90.75 10.93 90.75 C10.93 90.75 10.78 90.75 10.78 90.75 Z" class="st3"/></g>
      <g transform="translate(17.3649,-7.79098)"><path d="M0.37 84.16 C0.26 84.16 0.26 84.16 0.15 84.16 C0 84.16 0 84.31 0 84.42 C0 96.82 0 96.82 0 96.82 C0 96.93 -0 97.04 0.15 97.16 C10.93 103.22 10.93 103.22 10.93 103.22 C10.93 103.34 11.04 103.34 11.04 103.34 C11.15 103.34 11.15 103.34 11.26 103.22 C11.26 103.22 11.38 103.11 11.38 103 C11.38 90.6 11.38 90.6 11.38 90.6 C11.38 90.49 11.26 90.38 11.26 90.38 C0.49 84.16 0.49 84.16 0.49 84.16 C0.49 84.16 0.37 84.16 0.37 84.16 Z" class="st4"/></g>
      <g transform="translate(31.137,-7.79098)"><path d="M10.48 84.16 C10.48 84.16 10.37 84.16 10.37 84.31 C0.22 90.38 0.22 90.38 0.22 90.38 C0.11 90.49 0 90.6 0 90.71 C0 103 0 103 0 103 C0 103.11 0.11 103.22 0.22 103.22 C0.22 103.34 0.22 103.34 0.34 103.34 C0.34 103.34 0.45 103.34 0.45 103.22 C10.67 97.16 10.67 97.16 10.67 97.16 C10.78 97.04 10.78 96.93 10.78 96.82 C10.78 84.53 10.78 84.53 10.78 84.53 C10.78 84.42 10.78 84.31 10.67 84.31 C10.59 84.16 10.59 84.16 10.48 84.16 Z" class="st5"/></g>
    </g>
    <g transform="translate(10.3295,-23.8226)">
      <g transform="translate(4.19152,-3.23548)"><path d="M52.95 66 L52.95 103.34 L0 103.34 L0 66 L53.03 65.88 L52.95 66 L52.95 66 Z" class="st1"/></g>
      <g><path d="M56.21 58.99 C3.37 58.99 3.37 58.99 3.37 58.99 C1.5 58.99 -0 60.64 0 62.51 C0 99.97 0 99.97 0 99.97 C-0 101.84 1.5 103.34 3.37 103.34 C56.21 103.34 56.21 103.34 56.21 103.34 C58.05 103.34 59.88 101.84 59.88 99.97 C59.88 62.51 59.88 62.51 59.88 62.51 C59.88 60.64 58.05 58.99 56.21 58.99 ZM55.31 63.67 C55.31 98.69 55.31 98.69 55.31 98.69 C4.6 98.69 4.6 98.69 4.6 98.69 C4.6 63.67 4.6 63.67 4.6 63.67 C55.43 63.56 55.43 63.56 55.43 63.56 L55.31 63.67 Z" class="st2"/></g>
      <g transform="translate(19.1612,-22.7736)"><path d="M10.78 90.75 C10.67 90.75 10.67 90.75 10.55 90.75 C0.15 96.78 0.15 96.78 0.15 96.78 C-0 96.78 0 96.89 0 97.01 C0 97.12 -0 97.23 0.15 97.34 C10.67 103.34 10.67 103.34 10.67 103.34 C10.67 103.34 10.78 103.34 10.78 103.34 C10.93 103.34 10.93 103.34 11.04 103.34 C21.44 97.34 21.44 97.34 21.44 97.34 C21.56 97.34 21.56 97.23 21.56 97.12 C21.56 97.01 21.56 96.89 21.44 96.78 C10.93 90.75 10.93 90.75 10.93 90.75 C10.93 90.75 10.78 90.75 10.78 90.75 Z" class="st3"/></g>
      <g transform="translate(17.3649,-7.79098)"><path d="M0.37 84.16 C0.26 84.16 0.26 84.16 0.15 84.16 C0 84.16 0 84.31 0 84.42 C0 96.82 0 96.82 0 96.82 C0 96.93 -0 97.04 0.15 97.16 C10.93 103.22 10.93 103.22 10.93 103.22 C10.93 103.34 11.04 103.34 11.04 103.34 C11.15 103.34 11.15 103.34 11.26 103.22 C11.26 103.22 11.38 103.11 11.38 103 C11.38 90.6 11.38 90.6 11.38 90.6 C11.38 90.49 11.26 90.38 11.26 90.38 C0.49 84.16 0.49 84.16 0.49 84.16 C0.49 84.16 0.37 84.16 0.37 84.16 Z" class="st4"/></g>
      <g transform="translate(31.137,-7.79098)"><path d="M10.48 84.16 C10.48 84.16 10.37 84.16 10.37 84.31 C0.22 90.38 0.22 90.38 0.22 90.38 C0.11 90.49 0 90.6 0 90.71 C0 103 0 103 0 103 C0 103.11 0.11 103.22 0.22 103.22 C0.22 103.34 0.22 103.34 0.34 103.34 C0.34 103.34 0.45 103.34 0.45 103.22 C10.67 97.16 10.67 97.16 10.67 97.16 C10.78 97.04 10.78 96.93 10.78 96.82 C10.78 84.53 10.78 84.53 10.78 84.53 C10.78 84.42 10.78 84.31 10.67 84.31 C10.59 84.16 10.59 84.16 10.48 84.16 Z" class="st5"/></g>
    </g>
    <g transform="translate(25.3127,-14.6223)"><path d="M52.95 66 L52.95 103.34 L0 103.34 L0 66 L53.03 65.88 L52.95 66 L52.95 66 Z" class="st1"/></g>
    <g transform="translate(31.3006,0)"><path d="M28.74 91.95 C27.24 91.95 27.24 91.95 27.24 91.95 C13.1 91.95 13.1 91.95 13.1 91.95 C12.31 91.95 12.31 91.95 12.31 91.95 C14.26 98.8 11.6 99.82 0 99.82 C0 103.34 0 103.34 0 103.34 C14.86 103.34 14.86 103.34 14.86 103.34 C25.64 103.34 25.64 103.34 25.64 103.34 C39.52 103.34 39.52 103.34 39.52 103.34 C39.52 99.82 39.52 99.82 39.52 99.82 C27.96 99.82 26.8 98.8 28.74 91.95 Z" class="st6"/></g>
    <g transform="translate(21.1212,-11.3868)"><path d="M56.21 58.99 C3.37 58.99 3.37 58.99 3.37 58.99 C1.5 58.99 -0 60.64 0 62.51 C0 99.97 0 99.97 0 99.97 C-0 101.84 1.5 103.34 3.37 103.34 C56.21 103.34 56.21 103.34 56.21 103.34 C58.05 103.34 59.88 101.84 59.88 99.97 C59.88 62.51 59.88 62.51 59.88 62.51 C59.88 60.64 58.05 58.99 56.21 58.99 ZM55.31 63.67 C55.31 98.69 55.31 98.69 55.31 98.69 C4.6 98.69 4.6 98.69 4.6 98.69 C4.6 63.67 4.6 63.67 4.6 63.67 C55.43 63.56 55.43 63.56 55.43 63.56 L55.31 63.67 Z" class="st2"/></g>
    <g transform="translate(40.2824,-34.1605)"><path d="M10.78 90.75 C10.67 90.75 10.67 90.75 10.55 90.75 C0.15 96.78 0.15 96.78 0.15 96.78 C-0 96.78 0 96.89 0 97.01 C0 97.12 -0 97.23 0.15 97.34 C10.67 103.34 10.67 103.34 10.67 103.34 C10.67 103.34 10.78 103.34 10.78 103.34 C10.93 103.34 10.93 103.34 11.04 103.34 C21.44 97.34 21.44 97.34 21.44 97.34 C21.56 97.34 21.56 97.23 21.56 97.12 C21.56 97.01 21.56 96.89 21.44 96.78 C10.93 90.75 10.93 90.75 10.93 90.75 C10.93 90.75 10.78 90.75 10.78 90.75 Z" class="st3"/></g>
    <g transform="translate(38.486,-19.1778)"><path d="M0.37 84.16 C0.26 84.16 0.26 84.16 0.15 84.16 C0 84.16 -0 84.31 0 84.42 C0 96.82 0 96.82 0 96.82 C0 96.93 -0 97.04 0.15 97.16 C10.93 103.22 10.93 103.22 10.93 103.22 C10.93 103.34 11.04 103.34 11.04 103.34 C11.15 103.34 11.15 103.34 11.26 103.22 C11.26 103.22 11.38 103.11 11.38 103 C11.38 90.6 11.38 90.6 11.38 90.6 C11.38 90.49 11.26 90.38 11.26 90.38 C0.49 84.16 0.49 84.16 0.49 84.16 C0.49 84.16 0.37 84.16 0.37 84.16 Z" class="st4"/></g>
    <g transform="translate(52.2582,-19.1778)"><path d="M10.48 84.16 C10.48 84.16 10.37 84.16 10.37 84.31 C0.22 90.38 0.22 90.38 0.22 90.38 C0.11 90.49 0 90.6 0 90.71 C0 103 0 103 0 103 C0 103.11 0.11 103.22 0.22 103.22 C0.22 103.34 0.22 103.34 0.34 103.34 C0.34 103.34 0.45 103.34 0.45 103.22 C10.67 97.16 10.67 97.16 10.67 97.16 C10.78 97.04 10.78 96.93 10.78 96.82 C10.78 84.53 10.78 84.53 10.78 84.53 C10.78 84.42 10.78 84.31 10.67 84.31 C10.59 84.16 10.59 84.16 10.48 84.16 Z" class="st5"/></g>
  </g>
</svg>`;

const maskFromSVG = (svg: string): string => {
  return svg
    .split('#00bbf1').join('#ffffff')
    .split('#c5c5c5').join('#ffffff')
    .split('#e6f8fe').join('#ffffff')
    .split('#ccf1fc').join('#ffffff')
    .split('#80ddf8').join('#ffffff')
    .split('#919191').join('#ffffff');
};

type TopologyNodeIconTextures = {
  gatewayPoolIconTexture: THREE.Texture;
  workerSiteIconTexture: THREE.Texture;
  gatewayPoolMaskTexture: THREE.Texture;
  workerSiteMaskTexture: THREE.Texture;
};

type RenderTopologyNodeGlyphArgs = {
  glyph: 'router' | 'vm' | 'default';
  size: number;
  color: string;
  textures: TopologyNodeIconTextures;
  workerOffsetScale?: number;
};

export function getTopologyNodeGlyph(group?: string): 'router' | 'vm' | 'default' {
  if (group === 'pool' || group === 'gateway-node') {
    return 'router';
  }
  if (group === 'site' || group === 'worker-node') {
    return 'vm';
  }
  return 'default';
}

export function useTopologyNodeIconTextures(): TopologyNodeIconTextures {
  const gatewayPoolIconTexture = useMemo(() => {
    const texture = new THREE.TextureLoader().load(`data:image/svg+xml;utf8,${encodeURIComponent(gatewayPoolSvgIcon)}`);
    texture.colorSpace = THREE.SRGBColorSpace;
    texture.needsUpdate = true;
    return texture;
  }, []);

  const workerSiteIconTexture = useMemo(() => {
    const texture = new THREE.TextureLoader().load(`data:image/svg+xml;utf8,${encodeURIComponent(workerSiteSvgIcon)}`);
    texture.colorSpace = THREE.SRGBColorSpace;
    texture.needsUpdate = true;
    return texture;
  }, []);

  const gatewayPoolMaskTexture = useMemo(() => {
    const texture = new THREE.TextureLoader().load(`data:image/svg+xml;utf8,${encodeURIComponent(maskFromSVG(gatewayPoolSvgIcon))}`);
    texture.colorSpace = THREE.SRGBColorSpace;
    texture.needsUpdate = true;
    return texture;
  }, []);

  const workerSiteMaskTexture = useMemo(() => {
    const texture = new THREE.TextureLoader().load(`data:image/svg+xml;utf8,${encodeURIComponent(maskFromSVG(workerSiteSvgIcon))}`);
    texture.colorSpace = THREE.SRGBColorSpace;
    texture.needsUpdate = true;
    return texture;
  }, []);

  useEffect(() => {
    return () => {
      gatewayPoolIconTexture.dispose();
      workerSiteIconTexture.dispose();
      gatewayPoolMaskTexture.dispose();
      workerSiteMaskTexture.dispose();
    };
  }, [gatewayPoolIconTexture, workerSiteIconTexture, gatewayPoolMaskTexture, workerSiteMaskTexture]);

  return {
    gatewayPoolIconTexture,
    workerSiteIconTexture,
    gatewayPoolMaskTexture,
    workerSiteMaskTexture
  };
}

export function renderTopologyNodeGlyph({
  glyph,
  size,
  color,
  textures,
  workerOffsetScale = -0.16
}: RenderTopologyNodeGlyphArgs): React.ReactNode {
  if (glyph === 'router') {
    return (
      <group>
        <mesh position={[0, 0, 0.022]} renderOrder={10}>
          <planeGeometry args={[size * 2.28, size * 2.18]} />
          <meshBasicMaterial
            alphaMap={textures.gatewayPoolMaskTexture}
            color={color}
            transparent
            toneMapped={false}
            opacity={1}
            alphaTest={0.01}
            depthTest={false}
            depthWrite={false}
          />
        </mesh>
        <mesh position={[0, 0, 0.024]} renderOrder={11}>
          <planeGeometry args={[size * 2.0, size * 1.9]} />
          <meshBasicMaterial
            map={textures.gatewayPoolIconTexture}
            transparent
            toneMapped={false}
            opacity={1}
            alphaTest={0.01}
            depthTest={false}
            depthWrite={false}
          />
        </mesh>
      </group>
    );
  }

  if (glyph === 'vm') {
    return (
      <group>
        <mesh position={[size * workerOffsetScale, 0, 0.022]} renderOrder={10}>
          <planeGeometry args={[size * 2.48, size * 2.86]} />
          <meshBasicMaterial
            alphaMap={textures.workerSiteMaskTexture}
            color={color}
            transparent
            toneMapped={false}
            opacity={1}
            alphaTest={0.01}
            depthTest={false}
            depthWrite={false}
          />
        </mesh>
        <mesh position={[size * workerOffsetScale, 0, 0.024]} renderOrder={11}>
          <planeGeometry args={[size * 2.12, size * 2.5]} />
          <meshBasicMaterial
            map={textures.workerSiteIconTexture}
            transparent
            toneMapped={false}
            opacity={1}
            alphaTest={0.01}
            depthTest={false}
            depthWrite={false}
          />
        </mesh>
      </group>
    );
  }

  return null;
}
